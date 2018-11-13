/*
Copyright 2018 The Conductor Authors.

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

package compute

import (
	"context"
	"fmt"

	corev1alpha1 "github.com/upbound/conductor/pkg/apis/core/v1alpha1"
	gcpcomputev1alpha1 "github.com/upbound/conductor/pkg/apis/gcp/compute/v1alpha1"
	gcpv1alpha1 "github.com/upbound/conductor/pkg/apis/gcp/v1alpha1"
	"github.com/upbound/conductor/pkg/clients/gcp"
	"github.com/upbound/conductor/pkg/clients/gcp/gke"
	"github.com/upbound/conductor/pkg/util"
	"google.golang.org/api/container/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	controllerName    = "gke.compute.gcp.conductor.io"
	finalizer         = "finalizer." + controllerName
	clusterNamePrefix = "gke-"

	errorClusterClient           = "Failed to create cluster client"
	errorCreatingCluster         = "Failed to create new cluster"
	errorUpdatingCluster         = "Failed to update cluster"
	errorDeletingCluster         = "Failed to delete cluster"
	errorClusterConnectionSecret = "Failed to create/update cluster connection secret"
)

var (
	ctx           = context.Background()
	result        = reconcile.Result{}
	resultRequeue = reconcile.Result{Requeue: true}
)

// Add creates a new Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// Reconciler reconciles a Provider object
type Reconciler struct {
	client.Client
	scheme     *runtime.Scheme
	kubeclient kubernetes.Interface
	recorder   record.EventRecorder

	connect func(*gcpcomputev1alpha1.GKECluster) (gke.Client, error)
	create  func(*gcpcomputev1alpha1.GKECluster, gke.Client) (reconcile.Result, error)
	sync    func(*gcpcomputev1alpha1.GKECluster, gke.Client) (reconcile.Result, error)
	delete  func(*gcpcomputev1alpha1.GKECluster, gke.Client) (reconcile.Result, error)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	r := &Reconciler{
		Client:     mgr.GetClient(),
		scheme:     mgr.GetScheme(),
		kubeclient: kubernetes.NewForConfigOrDie(mgr.GetConfig()),
		recorder:   mgr.GetRecorder(controllerName),
	}
	r.connect = r._connect
	r.create = r._create
	r.sync = r._sync
	r.delete = r._delete
	return r
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(controllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to Provider
	err = c.Watch(&source.Kind{Type: &gcpcomputev1alpha1.GKECluster{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

// fail - helper function to set fail condition with reason and message
func (r *Reconciler) fail(instance *gcpcomputev1alpha1.GKECluster, reason, msg string) (reconcile.Result, error) {
	instance.Status.UnsetAllConditions()
	instance.Status.SetFailed(reason, msg)
	return resultRequeue, r.Update(context.TODO(), instance)
}

// connectionSecret return secret object for cluster instance
func (r *Reconciler) connectionSecret(instance *gcpcomputev1alpha1.GKECluster, cluster *container.Cluster) *corev1.Secret {
	secret := instance.ConnectionSecret()
	data := make(map[string][]byte)
	data[corev1alpha1.ResourceCredentialsSecretEndpointKey] = []byte(cluster.Endpoint)
	data[corev1alpha1.ResourceCredentialsSecretUserKey] = []byte(cluster.MasterAuth.Username)
	data[corev1alpha1.ResourceCredentialsSecretPasswordKey] = []byte(cluster.MasterAuth.Password)
	data[corev1alpha1.ResourceCredentialsSecretCAKey] = []byte(cluster.MasterAuth.ClusterCaCertificate)
	data[corev1alpha1.ResourceCredentialsSecretClientCertKey] = []byte(cluster.MasterAuth.ClientCertificate)
	data[corev1alpha1.ResourceCredentialsSecretClientKeyKey] = []byte(cluster.MasterAuth.ClientKey)
	secret.Data = data

	return secret
}

func (r *Reconciler) _connect(instance *gcpcomputev1alpha1.GKECluster) (gke.Client, error) {
	// Fetch Provider
	p := &gcpv1alpha1.Provider{}
	providerNamespacedName := types.NamespacedName{
		Namespace: instance.Namespace,
		Name:      instance.Spec.ProviderRef.Name,
	}
	err := r.Get(ctx, providerNamespacedName, p)
	if err != nil {
		return nil, err
	}

	// Check provider status
	if !p.IsValid() {
		return nil, fmt.Errorf("provider status is invalid")
	}

	creds, err := gcp.ProviderCredentials(r.kubeclient, p, gke.DefaultScope)
	if err != nil {
		return nil, err
	}

	return gke.NewClusterClient(creds)
}

func (r *Reconciler) _create(instance *gcpcomputev1alpha1.GKECluster, client gke.Client) (reconcile.Result, error) {
	clusterName := fmt.Sprintf("%s%s", clusterNamePrefix, instance.UID)

	_, err := client.CreateCluster(clusterName, instance.Spec)
	if err != nil && !gcp.IsAlreadyExists(err) {
		if gcp.IsBadRequest(err) {
			instance.Status.SetFailed(errorCreatingCluster, err.Error())
			// do not requeue on bad requests
			return result, r.Update(ctx, instance)
		}
		return r.fail(instance, errorCreatingCluster, err.Error())
	}

	instance.Status.UnsetAllConditions()
	instance.Status.SetCreating()
	instance.Status.ClusterName = clusterName

	return resultRequeue, r.Update(ctx, instance)
}

func (r *Reconciler) _sync(instance *gcpcomputev1alpha1.GKECluster, client gke.Client) (reconcile.Result, error) {
	cluster, err := client.GetCluster(instance.Spec.Zone, instance.Status.ClusterName)
	if err != nil {
		return r.fail(instance, errorUpdatingCluster, err.Error())
	}

	if cluster.Status != gcpcomputev1alpha1.ClusterStateRunning {
		return resultRequeue, nil
	}

	// create connection secret
	if _, err := util.ApplySecret(r.kubeclient, r.connectionSecret(instance, cluster)); err != nil {
		return r.fail(instance, errorClusterConnectionSecret, err.Error())
	}

	instance.Status.UnsetAllConditions()
	instance.Status.SetReady()

	// TODO: figure out how we going to handle cluster statuses other than RUNNING
	return result, r.Update(ctx, instance)

}

// _delete check reclaim policy and if needed delete the gke cluster resource
func (r *Reconciler) _delete(instance *gcpcomputev1alpha1.GKECluster, client gke.Client) (reconcile.Result, error) {
	if instance.Spec.ReclaimPolicy == corev1alpha1.ReclaimDelete {
		if err := client.DeleteCluster(instance.Spec.Zone, instance.Status.ClusterName); err != nil {
			return r.fail(instance, errorDeletingCluster, err.Error())
		}
	}
	util.RemoveFinalizer(&instance.ObjectMeta, finalizer)
	instance.Status.UnsetAllConditions()
	return result, r.Update(ctx, instance)
}

// Reconcile reads that state of the cluster for a Provider object and makes changes based on the state read
// and what is in the Provider.Spec
func (r *Reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the Provider instance
	instance := &gcpcomputev1alpha1.GKECluster{}
	err := r.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Create GKE Client
	client, err := r.connect(instance)
	if err != nil {
		return r.fail(instance, errorClusterClient, err.Error())
	}

	// Check for deletion
	if instance.DeletionTimestamp != nil {
		return r.delete(instance, client)
	}

	// Add finalizer
	if !util.HasFinalizer(&instance.ObjectMeta, finalizer) {
		util.AddFinalizer(&instance.ObjectMeta, finalizer)
		if err := r.Update(ctx, instance); err != nil {
			return resultRequeue, err
		}
	}

	// Create cluster instance
	if instance.Status.ClusterName == "" {
		return r.create(instance, client)
	}

	// Sync cluster instance status with cluster status
	return r.sync(instance, client)
}
