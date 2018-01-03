/*
Copyright 2016 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package pool to manage a rook pool.
package pool

import (
	"fmt"
	"reflect"

	"github.com/coreos/pkg/capnslog"
	opkit "github.com/rook/operator-kit"
	rookalpha "github.com/rook/rook/pkg/apis/rook.io/v1alpha1"
	"github.com/rook/rook/pkg/clusterd"
	ceph "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/model"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/client-go/tools/cache"
)

const (
	customResourceName       = "pool"
	customResourceNamePlural = "pools"
	replicatedType           = "replicated"
	erasureCodeType          = "erasure-coded"
)

var logger = capnslog.NewPackageLogger("github.com/rook/rook", "op-pool")

// PoolResource represents the Pool custom resource object
var PoolResource = opkit.CustomResource{
	Name:    customResourceName,
	Plural:  customResourceNamePlural,
	Group:   rookalpha.CustomResourceGroup,
	Version: rookalpha.Version,
	Scope:   apiextensionsv1beta1.NamespaceScoped,
	Kind:    reflect.TypeOf(rookalpha.Pool{}).Name(),
}

// PoolController represents a controller object for pool custom resources
type PoolController struct {
	context *clusterd.Context
}

// NewPoolController create controller for watching pool custom resources created
func NewPoolController(context *clusterd.Context) *PoolController {
	return &PoolController{
		context: context,
	}
}

// Watch watches for instances of Pool custom resources and acts on them
func (c *PoolController) StartWatch(namespace string, stopCh chan struct{}) error {

	resourceHandlerFuncs := cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onAdd,
		UpdateFunc: c.onUpdate,
		DeleteFunc: c.onDelete,
	}

	logger.Infof("start watching pool resources in namespace %s", namespace)
	watcher := opkit.NewWatcher(PoolResource, namespace, resourceHandlerFuncs, c.context.RookClientset.Rook().RESTClient())
	go watcher.Watch(&rookalpha.Pool{}, stopCh)
	return nil
}

func (c *PoolController) onAdd(obj interface{}) {
	pool := obj.(*rookalpha.Pool).DeepCopy()

	err := createPool(c.context, pool)
	if err != nil {
		logger.Errorf("failed to create pool %s. %+v", pool.ObjectMeta.Name, err)
	}
}

func (c *PoolController) onUpdate(oldObj, newObj interface{}) {
	oldPool := oldObj.(*rookalpha.Pool)
	pool := newObj.(*rookalpha.Pool)

	if oldPool.Name != pool.Name {
		logger.Errorf("failed to update pool %s. name update not allowed", pool.Name)
		return
	}
	if pool.Spec.ErasureCoded.CodingChunks != 0 && pool.Spec.ErasureCoded.DataChunks != 0 {
		logger.Errorf("failed to update pool %s. erasurecoded update not allowed", pool.Name)
		return
	}
	if !poolChanged(oldPool.Spec, pool.Spec) {
		logger.Debugf("pool %s not changed", pool.Name)
		return
	}

	// if the pool is modified, allow the pool to be created if it wasn't already
	logger.Infof("updating pool %s", pool.Name)
	if err := createPool(c.context, pool); err != nil {
		logger.Errorf("failed to create (modify) pool %s. %+v", pool.ObjectMeta.Name, err)
	}
}

func poolChanged(old, new rookalpha.PoolSpec) bool {
	if old.Replicated.Size != new.Replicated.Size {
		logger.Infof("pool replication changed from %d to %d", old.Replicated.Size, new.Replicated.Size)
		return true
	}
	return false
}

func (c *PoolController) onDelete(obj interface{}) {
	pool := obj.(*rookalpha.Pool)
	if err := deletePool(c.context, pool); err != nil {
		logger.Errorf("failed to delete pool %s. %+v", pool.ObjectMeta.Name, err)
	}
}

// Create the pool
func createPool(context *clusterd.Context, p *rookalpha.Pool) error {
	// validate the pool settings
	if err := ValidatePool(context, p); err != nil {
		return fmt.Errorf("invalid pool %s arguments. %+v", p.Name, err)
	}

	// create the pool
	logger.Infof("creating pool %s in namespace %s", p.Name, p.Namespace)
	if err := ceph.CreatePoolWithProfile(context, p.Namespace, *p.Spec.ToModel(p.Name), p.Name); err != nil {
		return fmt.Errorf("failed to create pool %s. %+v", p.Name, err)
	}

	logger.Infof("created pool %s", p.Name)
	return nil
}

// Delete the pool
func deletePool(context *clusterd.Context, p *rookalpha.Pool) error {

	if err := ceph.DeletePool(context, p.Namespace, p.Name); err != nil {
		return fmt.Errorf("failed to delete pool '%s'. %+v", p.Name, err)
	}

	return nil
}

// Check if the pool exists
func poolExists(context *clusterd.Context, p *rookalpha.Pool) (bool, error) {
	pools, err := ceph.GetPools(context, p.Namespace)
	if err != nil {
		return false, err
	}
	for _, pool := range pools {
		if pool.Name == p.Name {
			return true, nil
		}
	}
	return false, nil
}

func ModelToSpec(pool model.Pool) rookalpha.PoolSpec {
	ec := pool.ErasureCodedConfig
	return rookalpha.PoolSpec{
		FailureDomain: pool.FailureDomain,
		Replicated:    rookalpha.ReplicatedSpec{Size: pool.ReplicatedConfig.Size},
		ErasureCoded:  rookalpha.ErasureCodedSpec{CodingChunks: ec.CodingChunkCount, DataChunks: ec.DataChunkCount, Algorithm: ec.Algorithm},
	}
}

// Validate the pool arguments
func ValidatePool(context *clusterd.Context, p *rookalpha.Pool) error {
	if p.Name == "" {
		return fmt.Errorf("missing name")
	}
	if p.Namespace == "" {
		return fmt.Errorf("missing namespace")
	}
	if err := ValidatePoolSpec(context, p.Namespace, &p.Spec); err != nil {
		return err
	}
	return nil
}

func ValidatePoolSpec(context *clusterd.Context, namespace string, p *rookalpha.PoolSpec) error {
	if p.Replication() != nil && p.ErasureCode() != nil {
		return fmt.Errorf("both replication and erasure code settings cannot be specified")
	}
	if p.Replication() == nil && p.ErasureCode() == nil {
		return fmt.Errorf("neither replication nor erasure code settings were specified")
	}

	// validate the failure domain if specified
	if p.FailureDomain != "" {
		crush, err := ceph.GetCrushMap(context, namespace)
		if err != nil {
			return fmt.Errorf("failed to get crush map. %+v", err)
		}
		found := false
		for _, t := range crush.Types {
			if t.Name == p.FailureDomain {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unrecognized failure domain %s", p.FailureDomain)
		}
	}

	return nil
}
