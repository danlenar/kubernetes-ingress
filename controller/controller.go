// Copyright 2019 HAProxy Technologies LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"path/filepath"
	"strings"

	"github.com/haproxytech/client-native/v2/models"
	config "github.com/haproxytech/kubernetes-ingress/controller/configuration"
	"github.com/haproxytech/kubernetes-ingress/controller/haproxy/api"
	"github.com/haproxytech/kubernetes-ingress/controller/haproxy/process"
	"github.com/haproxytech/kubernetes-ingress/controller/ingress"
	"github.com/haproxytech/kubernetes-ingress/controller/route"
	"github.com/haproxytech/kubernetes-ingress/controller/store"
	"github.com/haproxytech/kubernetes-ingress/controller/utils"
	"k8s.io/apimachinery/pkg/watch"
)

// HAProxyController is ingress controller
type HAProxyController struct {
	Cfg            config.ControllerCfg
	crManager      CRManager
	Client         api.HAProxyClient
	OSArgs         utils.OSArgs
	Store          store.K8s
	PublishService *utils.NamespaceValue
	AuxCfgModTime  int64
	eventChan      chan SyncDataEvent
	ingressChan    chan ingress.Sync
	k8s            *K8s
	ready          bool
	reload         bool
	restart        bool
	updateHandlers []UpdateHandler
	haproxyProcess process.Process
	PodNamespace   string
	PodPrefix      string
}

// Wrapping a Native-Client transaction and commit it.
// Returning an error to let panic or log it upon the scenario.
func (c *HAProxyController) clientAPIClosure(fn func() error) (err error) {
	if err = c.Client.APIStartTransaction(); err != nil {
		return err
	}
	defer func() {
		c.Client.APIDisposeTransaction()
	}()
	if err = fn(); err != nil {
		return err
	}

	if err = c.Client.APICommitTransaction(); err != nil {
		return err
	}
	return nil
}

// Start initializes and runs HAProxyController
func (c *HAProxyController) Start() {
	var err error
	logger.SetLevel(c.OSArgs.LogLevel.LogLevel)

	// Initialize controller
	err = c.Cfg.Init()
	if err != nil {
		logger.Panic(err)
	}

	c.Client, err = api.Init(c.Cfg.Env.CfgDir, c.Cfg.Env.MainCFGFile, c.Cfg.Env.HAProxyBinary, c.Cfg.Env.RuntimeSocket)
	if err != nil {
		logger.Panic(err)
	}

	c.initHandlers()
	c.haproxyStartup()

	// Controller PublishService
	parts := strings.Split(c.OSArgs.PublishService, "/")
	if len(parts) == 2 {
		c.PublishService = &utils.NamespaceValue{
			Namespace: parts[0],
			Name:      parts[1],
		}
	}

	// Get K8s client
	c.k8s, err = GetKubernetesClient(c.OSArgs.DisableServiceExternalName)
	if c.OSArgs.External {
		kubeconfig := filepath.Join(utils.HomeDir(), ".kube", "config")
		if c.OSArgs.KubeConfig != "" {
			kubeconfig = c.OSArgs.KubeConfig
		}
		c.k8s, err = GetRemoteKubernetesClient(kubeconfig, c.OSArgs.DisableServiceExternalName)
	}
	if err != nil {
		logger.Panic(err)
	}
	x := c.k8s.API.Discovery()
	if k8sVersion, err := x.ServerVersion(); err != nil {
		logger.Panicf("Unable to get Kubernetes version: %v\n", err)
	} else {
		logger.Printf("Running on Kubernetes version: %s %s", k8sVersion.String(), k8sVersion.Platform)
	}

	// Monitor k8s events
	c.eventChan = make(chan SyncDataEvent, watch.DefaultChanSize*6)
	go c.monitorChanges()
	if c.PublishService != nil {
		// Update Ingress status
		c.ingressChan = make(chan ingress.Sync, watch.DefaultChanSize*6)
		go ingress.UpdateStatus(c.k8s.API, c.Store, c.OSArgs.IngressClass, c.OSArgs.EmptyIngressClass, c.ingressChan)
	}
}

// Stop handles shutting down HAProxyController
func (c *HAProxyController) Stop() {
	logger.Infof("Stopping Ingress Controller")
	logger.Error(c.haproxyService("stop"))
}

// updateHAProxy is the control loop syncing HAProxy configuration
func (c *HAProxyController) updateHAProxy() {
	var reload bool
	var err error
	logger.Trace("HAProxy config sync started")

	err = c.Client.APIStartTransaction()
	if err != nil {
		logger.Error(err)
		return
	}
	defer func() {
		c.Client.APIDisposeTransaction()
	}()

	reload, restart := c.handleGlobalConfig()
	c.reload = c.reload || reload
	c.restart = c.restart || restart

	if len(route.CustomRoutes) != 0 {
		logger.Error(route.CustomRoutesReset(c.Client))
	}

	for _, namespace := range c.Store.Namespaces {
		if !namespace.Relevant {
			continue
		}
		for _, ingResource := range namespace.Ingresses {
			i := ingress.New(c.Store, ingResource, c.OSArgs.IngressClass, c.OSArgs.EmptyIngressClass)
			if i == nil {
				logger.Debugf("ingress '%s/%s' ignored: no matching IngressClass", ingResource.Namespace, ingResource.Name)
				continue
			}
			if c.PublishService != nil && ingResource.Status == ADDED {
				select {
				case c.ingressChan <- ingress.Sync{Ingress: ingResource}:
				default:
					logger.Errorf("Ingress %s/%s: unable to sync status: sync channel full", ingResource.Namespace, ingResource.Name)
				}
			}
			c.reload = i.Update(c.Store, &c.Cfg, c.Client) || c.reload
		}
	}

	for _, handler := range c.updateHandlers {
		reload, err = handler.Update(c.Store, &c.Cfg, c.Client)
		logger.Error(err)
		c.reload = c.reload || reload
	}

	err = c.Client.APICommitTransaction()
	if err != nil {
		logger.Error("unable to Sync HAProxy configuration !!")
		logger.Error(err)
		c.clean(true)
		return
	}

	if !c.ready {
		c.setToReady()
	}

	switch {
	case c.restart:
		if err = c.haproxyService("restart"); err != nil {
			logger.Error(err)
		} else {
			logger.Info("HAProxy restarted")
		}
	case c.reload:
		if err = c.haproxyService("reload"); err != nil {
			logger.Error(err)
		} else {
			logger.Info("HAProxy reloaded")
		}
	}

	c.clean(false)

	logger.Trace("HAProxy config sync ended")
}

// setToRready exposes readiness endpoint
func (c *HAProxyController) setToReady() {
	logger.Panic(c.clientAPIClosure(func() error {
		return c.Client.FrontendBindEdit("healthz",
			models.Bind{
				Name:    "v4",
				Address: "0.0.0.0:1042",
			})
	}))
	if !c.OSArgs.DisableIPV6 {
		logger.Panic(c.clientAPIClosure(func() error {
			return c.Client.FrontendBindCreate("healthz",
				models.Bind{
					Name:    "v6",
					Address: ":::1042",
					V4v6:    true,
				})
		}))
	}
	logger.Debugf("healthz frontend exposed for readiness probe")
	cm := c.Store.ConfigMaps.Main
	if cm.Name != "" && !cm.Loaded {
		logger.Warningf("Main configmap '%s/%s' not found", cm.Namespace, cm.Name)
	}
	c.ready = true
}

// clean controller state
func (c *HAProxyController) clean(failedSync bool) {
	logger.Error(c.Cfg.Clean())
	c.Cfg.SSLPassthrough = false
	if !failedSync {
		c.Store.Clean()
	}
	c.reload = false
	c.restart = false
}
