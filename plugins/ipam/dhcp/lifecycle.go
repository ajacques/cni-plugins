package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	kapiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func SetNodeIsOfflineState(clientset *kubernetes.Clientset, value bool) error {
	nodename := os.Getenv("NODENAME")

	var condition kapiv1.NodeCondition

	if value {
		condition = kapiv1.NodeCondition{
			Type:               kapiv1.NodeNetworkUnavailable,
			Status:             kapiv1.ConditionTrue,
			Reason:             "DHCPIsDown",
			Message:            "DHCP Daemon is shutting down on this node",
			LastTransitionTime: metav1.Now(),
			LastHeartbeatTime:  metav1.Now(),
		}
	} else {
		condition = kapiv1.NodeCondition{
			Type:               kapiv1.NodeNetworkUnavailable,
			Status:             kapiv1.ConditionFalse,
			Reason:             "DHCPIsUp",
			Message:            "DHCP Daemon is running on this node",
			LastTransitionTime: metav1.Now(),
			LastHeartbeatTime:  metav1.Now(),
		}
	}
	raw, err := json.Marshal(&[]kapiv1.NodeCondition{condition})
	if err != nil {
		return err
	}
	patch := []byte(fmt.Sprintf(`{"status":{"conditions":%s}}`, raw))

	_, err = clientset.CoreV1().Nodes().PatchStatus(context.Background(), nodename, patch)
	if err != nil {
		return err
	}
	return nil
}

func shutdown() {
	if config, err := rest.InClusterConfig(); err == nil {
		config.Timeout = 2 * time.Second

		// Create the k8s clientset.
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			fmt.Printf("failed to connect to Kubernetes: %v", err)
			return
		}

		err = SetNodeIsOfflineState(clientset, true)
		if err != nil {
			fmt.Printf("failed to connect to Kubernetes: %v", err)
			return
		}
	}
}
