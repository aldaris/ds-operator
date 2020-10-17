/*
   skeleton DS controller
*/

package controllers

import (
	"context"
	"fmt"
	"time"

	directoryv1alpha1 "github.com/ForgeRock/ds-operator/api/v1alpha1"
	ldap "github.com/ForgeRock/ds-operator/pkg/ldap"
	"github.com/go-logr/logr"
	"github.com/prometheus/common/log"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DirectoryServiceReconciler reconciles a DirectoryService object
type DirectoryServiceReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

var (
	// requeue the request
	requeue = ctrl.Result{RequeueAfter: time.Second * 30}
)

// Reconcile loop for DS controller
// Add in all the RBAC permissions that a DS controller needs. StatefulSets, etc.
// +kubebuilder:rbac:groups=directory.forgerock.io,resources=directoryservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=directory.forgerock.io,resources=directoryservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets/status,verbs=get,update,patch,delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
func (r *DirectoryServiceReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	// This adds the log data to every log line
	var log = r.Log.WithValues("directoryservice", req.NamespacedName)

	log.Info("Reconcile")

	var ds directoryv1alpha1.DirectoryService

	// Load the DirectoryService
	if err := r.Get(ctx, req.NamespacedName, &ds); err != nil {
		log.Info("unable to fetch DirectorService. You can probably ignore this..")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// finalizer hooks..
	// This registers finalizers for deleting the object
	myFinalizerName := "directory.finalizers.forgerock.io"

	// examine DeletionTimestamp to determine if object is under deletion
	if ds.ObjectMeta.DeletionTimestamp.IsZero() {
		log.V(3).Info("Registering finalizer for Directory Service", "name", ds.Name)
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !containsString(ds.GetFinalizers(), myFinalizerName) {
			ds.SetFinalizers(append(ds.GetFinalizers(), myFinalizerName))
			if err := r.Update(context.Background(), &ds); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		log.Info("Deleting Directory Service", "name", ds.Name)
		// The object is being deleted
		if containsString(ds.GetFinalizers(), myFinalizerName) {
			// our finalizer is present, so lets handle any external dependency
			if err := r.deleteExternalResources(&ds); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			ds.SetFinalizers(removeString(ds.GetFinalizers(), myFinalizerName))
			if err := r.Update(context.Background(), &ds); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	//// SECRETS ////
	if res, err := r.reconcileSecrets(ctx, &ds); err != nil {
		return res, err
	}

	//// StatefulSets ////
	if res, err := r.reconcileSTS(ctx, &ds); err != nil {
		return res, err
	}

	//// Services ////
	_, err := r.reconcileService(ctx, &ds)
	if err != nil {
		return requeue, err
	}

	//// LDAP Updates
	ldap, err := r.getAdminLDAPConnection(ctx, &ds)
	// server may be down or coming up. Reque
	if err != nil {
		return requeue, nil
	}
	defer ldap.Close()

	// update ldap service account passwords
	if err := r.updatePasswords(ctx, &ds, ldap); err != nil {
		return requeue, nil
	}

	// Update backup / restore options
	if err := r.updateBackup(ctx, &ds, ldap); err != nil {
		return requeue, nil
	}

	// Get the LDAP backup status
	if err := r.updateBackupStatus(ctx, &ds, ldap); err != nil {
		log.Info("Could not get backup status", "err", err)
		// todo: We still want to update the remaining status....
	}

	// Update the status of our ds object
	if err := r.Status().Update(ctx, &ds); err != nil {
		log.Error(err, "unable to update Directory status")
		return ctrl.Result{}, err
	}

	log.V(4).Info("Returning from Reconcile")

	return requeue, nil
}

// SetupWithManager stuff
func (r *DirectoryServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&directoryv1alpha1.DirectoryService{}).
		Complete(r)
}

func (r *DirectoryServiceReconciler) deleteExternalResources(ds *directoryv1alpha1.DirectoryService) error {
	//
	// delete any external resources associated with the ds set
	//
	// Ensure that delete implementation is idempotent and safe to invoke
	// multiple times for same object.
	return nil
}

// Helper functions to check and remove string from a slice of strings.
func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return
}

func (r *DirectoryServiceReconciler) getAdminLDAPConnection(ctx context.Context, ds *directoryv1alpha1.DirectoryService) (*ldap.DSConnection, error) {
	// TODO: is there a more reliable way of getting the service hostname?
	// url := fmt.Sprintf("ldap://%s.%s.svc.cluster.local:1389", svc.Name, svc.Namespace)
	// For local testing we need to run kube port-forward and localhost...
	url := fmt.Sprintf("ldap://localhost:1389")

	// lookup the admin password. Do we want to cache this?
	var adminSecret v1.Secret
	account := ds.Spec.Passwords["uid=admin"]
	name := types.NamespacedName{Namespace: ds.Namespace, Name: account.SecretName}

	if err := r.Get(ctx, name, &adminSecret); err != nil {
		log.Error(err, "Can't find secret for the admin password", "secret", name)
		return nil, fmt.Errorf("Can't find the admin ldap secret")
	}

	password := adminSecret.Data[account.Key]

	ldap := ldap.DSConnection{DN: "uid=admin", URL: url, Password: string(password[:])}

	if err := ldap.Connect(); err != nil {
		r.Log.Info("Can't connect to ldap server, will try again later", "url", url, "err", err)
		return nil, err
	}

	return &ldap, nil

}
