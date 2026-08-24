package main

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	controller "github.com/gke-labs/dra-drivers/dra-driver-image-configurator/internal/controller"
	"k8s.io/client-go/kubernetes"
	resourceslice "k8s.io/dynamic-resource-allocation/resourceslice"
)

func main() {
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("setup")

	// Start the DRA ResourceSlice controller before manager
	config := ctrl.GetConfigOrDie()
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to create kubeClient: %v\n", err)
		os.Exit(1)
	}

	// Start the DRA ResourceSlice controller before manager
	_, err = resourceslice.StartController(
		context.Background(),
		resourceslice.Options{
			DriverName: controller.DriverName,
			KubeClient: kubeClient,
			Resources:  makeDriverResources(),
		},
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to start ResourceSlice controller: %v\n", err)
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		LeaderElection:   true,
		LeaderElectionID: controller.DriverName,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				// Cache only pods that the scheduler has nominated to a node.
				&corev1.Pod{}: {
					Field: fields.OneTermNotEqualSelector("status.nominatedNodeName", ""),
				},
			},
		},
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.PodReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorder(controller.DriverName),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller")
		os.Exit(1)
	}

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}

// makeDriverResources returns DriverResources for publishing ResourceSlices.
func makeDriverResources() *resourceslice.DriverResources {
	return &resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			"all-nodes": {
				AllNodes: true,
				Slices: []resourceslice.Slice{
					{
						Devices: []v1.Device{
							{
								Name:                     "image-configurator",
								AllowMultipleAllocations: new(true),
								BindsToNode:              new(false),
								BindingConditions:        []string{controller.BindingConditionUpdateImage},
								BindingFailureConditions: []string{controller.BindingFailureConditionUpdateImage},
							},
						},
					},
				},
			},
		},
	}
}
