package controllers

import (
	"context"
	"fmt"
	benzaiten "github.com/charmelionag/cloudcontroller/api/v1"
	"github.com/go-logr/logr"
	"google.golang.org/api/container/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"strings"
	"time"
)

type GCPKubernetesClusterReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	eventRecorder record.EventRecorder
	cloud         CloudProviders
}

func updateStatus(toLog bool, logger logr.Logger, cluster *benzaiten.GCPKubernetesCluster, cs benzaiten.ClusterStatus, msg string, keysAndValues ...any) {
	if toLog {
		logger.Info(msg, keysAndValues...)
	}
	cluster.Status = benzaiten.GCPKubernetesClusterStatus{
		Phase: cs,
	}
}

func (cr *GCPKubernetesClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("gcpkubernetescluster", req.NamespacedName)
	gkcCR := benzaiten.GCPKubernetesCluster{}
	err := cr.Get(ctx, req.NamespacedName, &gkcCR)
	if err != nil {
		if kerr.IsNotFound(err) {
			logger.Info("gcpkubernetescluster not found")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// does cluster exist in GCP?
	gkc, err := cr.cloud.GCP.GetCluster(gkcCR.Spec.Zone, gkcCR.Spec.ClusterName)
	if err != nil && notFoundGCPResource(err) {
		// cluster does not exist in GCP
		logger.Info("gcpkubernetescluster not found, creating cluster...")
		op, err := cr.cloud.GCP.CreateCluster(gkcCR.Spec.Zone, &container.Cluster{
			Name:             gkcCR.Spec.ClusterName,
			InitialNodeCount: gkcCR.Spec.InitialNodeCount,
		})
		if err != nil {
			logger.Error(err, "error creating gcpkubernetescluster")
			return ctrl.Result{}, err
		}
		// create cluster
		cr.eventRecorder.Event(&gkcCR, "Normal", "ClusterCreation", "GCP Kubernetes Cluster creating")
		updateStatus(true, logger, &gkcCR, benzaiten.ClusterStatusProvisioning, "GCP Kubernetes Cluster creating", "gcpkubernetescluster", gkcCR.Name, "operation", op)
		err = cr.Status().Update(ctx, &gkcCR)
		if err != nil {
			logger.Error(err, "error updating gcpkubernetescluster status")
			return ctrl.Result{}, err
		}
		// wait for cluster running state
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				gkcCreated, err := cr.cloud.GCP.GetCluster(gkcCR.Spec.Zone, gkcCR.Spec.ClusterName)
				if err != nil {
					logger.Error(err, "error verifying cluster status", "gcpkubernetescluster", gkcCR)
					return ctrl.Result{}, err
				}
				// update cluster status
				cr.eventRecorder.Event(&gkcCR, "Normal", "ClusterProvisioning", "GCP Kubernetes Cluster provisioning")
				updateStatus(false, logger, &gkcCR, benzaiten.ClusterStatusProvisioning, "GCP Kubernetes Cluster provisioning", "gcpkubernetescluster", gkcCR.Name)
				err = cr.Status().Update(ctx, &gkcCR)
				if err != nil {
					logger.Error(err, "error updating gcpkubernetescluster status")
					return ctrl.Result{}, err
				}
				if gkcCreated.Status == string(benzaiten.ClusterStatusRunning) {
					// cluster is running
					cr.eventRecorder.Event(&gkcCR, "Normal", "ClusterRunning", "GCP Kubernetes Cluster running")
					updateStatus(true, logger, &gkcCR, benzaiten.ClusterStatusRunning, "GCP Kubernetes Cluster running", "gcpkubernetescluster", gkcCR.Name)
					err = cr.Status().Update(ctx, &gkcCR)
					if err != nil {
						logger.Error(err, "error updating gcpkubernetescluster status")
						return ctrl.Result{}, err
					}
					return ctrl.Result{}, nil
				} else if gkcCreated.Status == string(benzaiten.ClusterStatusError) || gkcCreated.Status == string(benzaiten.ClusterStatusDegraded) || gkcCreated.Status == string(benzaiten.ClusterStatusUnspecified) {
					// cluster is in error
					cr.eventRecorder.Event(&gkcCR, "Warning", "ClusterNotRunning", fmt.Sprintf("GCP Kubernetes Cluster status is not running: %s", gkcCreated.Status))
					updateStatus(true, logger, &gkcCR, benzaiten.ClusterStatusError, "GCP Kubernetes Cluster not running", "gcpkubernetescluster", gkcCR.Name, "status", gkcCreated.Status)
					err = cr.Status().Update(ctx, &gkcCR)
					if err != nil {
						logger.Error(err, "error updating gcpkubernetescluster status")
						return ctrl.Result{}, err
					}
					return ctrl.Result{}, nil
				}
			case <-ctx.Done():
				return ctrl.Result{}, ctx.Err()
			}
		}

		return ctrl.Result{}, nil
	} else if err != nil {
		logger.Error(err, "error getting gcpkubernetescluster")
		return ctrl.Result{}, err
	}

	// detect cluster state
	gkc.Status = string(benzaiten.ClusterStatusRunning)
	cr.eventRecorder.Event(&gkcCR, "Normal", "ClusterDetection", "GCP Kubernetes Cluster found")
	updateStatus(true, logger, &gkcCR, benzaiten.ClusterStatus(gkc.Status), "GCP Kubernetes Cluster found", "gcpkubernetescluster", gkcCR.Name, "status", gkc.Status)
	err = cr.Status().Update(ctx, &gkcCR)
	if err != nil {
		logger.Error(err, "error updating gcpkubernetescluster status")
		return ctrl.Result{}, err
	}

	// synchronize changes if exists
	logger.Info("gcpkubernetescluster found, synchronizing...", "gcpkubernetescluster", gkc.Name)

	logger.Info("gcp kubernetes cluster reconciled")
	return ctrl.Result{RequeueAfter: time.Second * 60}, nil
}

func (cr *GCPKubernetesClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&benzaiten.GCPKubernetesCluster{}).
		Complete(cr)
}

func setupGCPKubernetesClusterController(mgr manager.Manager, cp CloudProviders) error {
	eventRecorder := mgr.GetEventRecorderFor("gcpkubernetescluster")
	cc := GCPKubernetesClusterReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		eventRecorder: eventRecorder,
		cloud:         cp,
	}

	// create GCPKubernetesCluster controller
	err := cc.SetupWithManager(mgr)
	if err != nil {
		return fmt.Errorf("unable to create GCPKubernetesCluster controller: %w", err)
	}

	return nil
}

func notFoundGCPResource(err error) bool {
	return strings.Split(fmt.Sprintf("%v", err), ":")[1] == " Error 404"
}
