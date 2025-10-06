/*
2024 NVIDIA CORPORATION & AFFILIATES

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"context"
	"log"
	"strings"

	"github.com/NVIDIA/k8s-operator-libs/pkg/crdutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	clientConfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/Mellanox/network-operator/pkg/config"
)

func main() {
	// Remove Whereabouts annotations and labels
	updateWhereaboutsDs()

	// Run CRD ensure logic at the end
	crdutil.EnsureCRDsCmd()
}

func updateWhereaboutsDs() {
	namespace := config.FromEnv().State.NetworkOperatorResourceNamespace

	ctx := context.Background()
	cfg, err := clientConfig.GetConfig()
	if err != nil {
		log.Fatalf("Failed to get Kubernetes config: %v", err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Find Whereabouts DaemonSets in Network Operator namespace
	dsList, err := client.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "tier=node",
	})
	if err != nil {
		log.Fatalf("Failed to list DaemonSets in namespace %q: %v", namespace, err)
	}

	for _, ds := range dsList.Items {
		appLabel := ds.Labels["app"]
		if !strings.HasPrefix(appLabel, "whereabouts") {
			continue
		}

		modified := false

		// Remove owner references
		if len(ds.OwnerReferences) > 0 {
			ds.OwnerReferences = nil
			modified = true
			log.Printf("Removed OwnerReferences: %s/%s\n", ds.Namespace, ds.Name)
		}

		// === Step 3: Remove annotation ===
		if ds.Annotations != nil {
			if _, ok := ds.Annotations["nvidia.network-operator.revision"]; ok {
				delete(ds.Annotations, "nvidia.network-operator.revision")
				modified = true
				log.Printf("Removed annotation: %s/%s\n", ds.Namespace, ds.Name)
			}
		}

		// Remove specific label
		if ds.Labels != nil {
			if val, ok := ds.Labels["nvidia.network-operator.state"]; ok && val == "state-whereabouts-cni" {
				delete(ds.Labels, "nvidia.network-operator.state")
				modified = true
				log.Printf("Removed label: %s/%s\n", ds.Namespace, ds.Name)
			}
		}

		// Update only if modified
		if modified {
			_, err := client.AppsV1().DaemonSets(ds.Namespace).Update(ctx, &ds, metav1.UpdateOptions{})
			if err != nil {
				log.Fatalf("Failed to update DaemonSet %s/%s: %v", ds.Namespace, ds.Name, err)
			}
		}
	}
}
