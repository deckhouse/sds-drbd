/*
Copyright 2023 Flant JSC

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

package controller

import (
	"context"
	"fmt"
	"net"
	sdsapi "sds-drbd-controller/api/v1alpha1"
	"strings"
	"time"

	lclient "github.com/LINBIT/golinstor/client"
	"github.com/go-logr/logr"
	"gopkg.in/yaml.v3"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	LinstorNodeControllerName = "linstor-node-controller"
	LinstorControllerType     = "CONTROLLER"
	LinstorSatelliteType      = "SATELLITE"
	LinstorOnlineStatus       = "ONLINE"
	LinstorOfflineStatus      = "OFFLINE"
	LinstorNodePort           = 3367  //
	LinstorEncryptionType     = "SSL" // "Plain"
	reachableTimeout          = 10 * time.Second
	DRBDNodeSelectorKey       = "storage.deckhouse.io/sds-drbd-node"
)

func NewLinstorNode(
	ctx context.Context,
	mgr manager.Manager,
	lc *lclient.Client,
	configSecretName string,
	interval int,
) (controller.Controller, error) {
	cl := mgr.GetClient()
	log := mgr.GetLogger()

	c, err := controller.New(LinstorNodeControllerName, mgr, controller.Options{
		Reconciler: reconcile.Func(func(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {

			if request.Name == configSecretName {
				log.Info("Start reconcile of LINSTOR nodes.")
				drbdNodeSelector := map[string]string{DRBDNodeSelectorKey: ""}
				err := reconcileLinstorNodes(ctx, cl, lc, log, request.Namespace, request.Name, drbdNodeSelector)
				if err != nil {
					log.Error(nil, "Failed reconcile of LINSTOR nodes")
				} else {
					log.Info("END reconcile of LINSTOR nodes.")
				}
			}

			return reconcile.Result{
				RequeueAfter: time.Duration(interval) * time.Second,
			}, nil

		}),
	})

	if err != nil {
		return nil, err
	}

	err = c.Watch(source.Kind(mgr.GetCache(), &v1.Secret{}), &handler.EnqueueRequestForObject{})

	return c, err

}

func reconcileLinstorNodes(ctx context.Context, cl client.Client, lc *lclient.Client, log logr.Logger, secretNamespace string, secretName string, drbdNodeSelector map[string]string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, reachableTimeout)
	defer cancel()

	configSecret, err := GetKubernetesSecretByName(ctx, cl, secretName, secretNamespace)
	if err != nil {
		log.Error(err, "Failed get secret:"+secretNamespace+"/"+secretName)
		return err
	}

	configNodeSelector, err := GetNodeSelectorFromConfig(*configSecret)
	if err != nil {
		log.Error(err, "Failed get node selector from secret:"+secretName+"/"+secretNamespace)
		return err
	}
	selectedKubernetesNodes, err := GetKubernetesNodesBySelector(ctx, cl, configNodeSelector)
	if err != nil {
		log.Error(err, "Failed get nodes from Kubernetes by selector:"+fmt.Sprint(configNodeSelector))
		return err
	}

	linstorSatelliteNodes, linstorControllerNodes, err := GetLinstorNodes(timeoutCtx, lc)
	if err != nil {
		log.Error(err, "Failed get LINSTOR nodes")
		return err
	}

	if len(selectedKubernetesNodes.Items) != 0 {
		err = AddOrConfigureDRBDNodes(ctx, cl, lc, log, selectedKubernetesNodes, linstorSatelliteNodes, drbdNodeSelector)
		if err != nil {
			log.Error(err, "Failed add DRBD nodes:")
			return err
		}
	} else {
		log.Info("reconcileLinstorNodes: There are not any Kubernetes nodes for LINSTOR that can be selected by selector:" + fmt.Sprint(configNodeSelector)) //TODO: log.Warn
	}

	// Remove logic
	allKubernetesNodes, err := GetAllKubernetesNodes(ctx, cl)
	if err != nil {
		log.Error(err, "Failed get all nodes from Kubernetes")
		return err
	}
	drbdNodesToRemove := DiffNodeLists(allKubernetesNodes, selectedKubernetesNodes)

	err = removeDRBDNodes(log, drbdNodesToRemove, linstorSatelliteNodes, drbdNodeSelector)
	if err != nil {
		log.Error(err, "Failed remove DRBD nodes:")
		return err
	}

	err = removeLinstorControllerNodes(ctx, lc, log, linstorControllerNodes)
	if err != nil {
		log.Error(err, "Failed remove LINSTOR controller nodes:")
		return err
	}

	return nil
}

func removeDRBDNodes(log logr.Logger, drbdNodesToRemove v1.NodeList, linstorSatelliteNodes []lclient.Node, drbdNodeSelector map[string]string) error {

	for _, drbdNodeToRemove := range drbdNodesToRemove.Items {

		for _, linstorNode := range linstorSatelliteNodes {
			if drbdNodeToRemove.Name == linstorNode.Name {
				// #TODO: Should we add ConfigureDRBDNode here?
				log.Info("Remove LINSTOR node: " + drbdNodeToRemove.Name)
				log.Error(nil, "Warning! Delete logic not yet implemented. Removal of LINSTOR nodes is prohibited.")
				break
			}
		}

		if labels.Set(drbdNodeSelector).AsSelector().Matches(labels.Set(drbdNodeToRemove.Labels)) {
			log.Info("Kubernetes node: " + drbdNodeToRemove.Name + "  have drbd label. Unset it")
			log.Error(nil, "Warning! Delete logic not yet implemented. Removal of LINSTOR nodes is prohibited.")
			// #TODO: How get current replica count and modify it?
		}

	}
	return nil
}

func AddOrConfigureDRBDNodes(ctx context.Context, cl client.Client, lc *lclient.Client, log logr.Logger, selectedKubernetesNodes v1.NodeList, linstorNodes []lclient.Node, drbdNodeSelector map[string]string) error {

	for _, selectedKubernetesNode := range selectedKubernetesNodes.Items {

		drbdNodeProperties := KubernetesNodeLabelsToProperties(selectedKubernetesNode.Labels)
		findMatch := false

		for _, linstorNode := range linstorNodes {
			if selectedKubernetesNode.Name == linstorNode.Name {
				findMatch = true
				err := ConfigureDRBDNode(ctx, lc, linstorNode, drbdNodeProperties)
				if err != nil {
					return fmt.Errorf("unable set drbd properties to node %s: %w", linstorNode.Name, err)
				}
				break
			}
		}

		if !labels.Set(drbdNodeSelector).AsSelector().Matches(labels.Set(selectedKubernetesNode.Labels)) {

			originalNode := selectedKubernetesNode.DeepCopy()
			newNode := selectedKubernetesNode.DeepCopy()
			if newNode.Labels == nil {
				newNode.Labels = make(map[string]string, len(drbdNodeSelector))
			}
			for labelKey, labelValue := range drbdNodeSelector {
				newNode.Labels[labelKey] = labelValue
			}

			err := cl.Patch(ctx, newNode, client.MergeFrom(originalNode))
			if err != nil {
				return fmt.Errorf("unable set drbd labels to node %s: %w", selectedKubernetesNode.Name, err)
			}
		}

		if !findMatch {
			log.Info("AddOrConfigureDRBDNodes: Create LINSTOR node: " + selectedKubernetesNode.Name)
			err := CreateDRBDNode(ctx, lc, selectedKubernetesNode, drbdNodeProperties)
			if err != nil {
				return fmt.Errorf("unable to create LINSTOR node %s: %w", selectedKubernetesNode.Name, err)
			}
		}

	}
	return nil
}

func ConfigureDRBDNode(ctx context.Context, lc *lclient.Client, linstorNode lclient.Node, drbdNodeProperties map[string]string) error {
	needUpdate := false

	for newPropertyName, newPropertyValue := range drbdNodeProperties {
		existingProperyValue, exists := linstorNode.Props[newPropertyName]
		if !exists || existingProperyValue != newPropertyValue {
			needUpdate = true
			break
		}
	}

	var propertiesToDelete []string

	for existingPropertyName := range linstorNode.Props {
		if !strings.HasPrefix(existingPropertyName, "Aux/") {
			continue
		}

		_, exist := drbdNodeProperties[existingPropertyName]
		if !exist {
			propertiesToDelete = append(propertiesToDelete, existingPropertyName)
		}

	}

	if needUpdate || len(propertiesToDelete) != 0 {
		err := lc.Nodes.Modify(ctx, linstorNode.Name, lclient.NodeModify{
			GenericPropsModify: lclient.GenericPropsModify{
				OverrideProps: drbdNodeProperties,
				DeleteProps:   propertiesToDelete,
			},
		})
		if err != nil {
			return fmt.Errorf("unable to update node properties: %w", err)
		}
	}
	return nil
}

func CreateDRBDNode(ctx context.Context, lc *lclient.Client, selectedKubernetesNode v1.Node, drbdNodeProperties map[string]string) error {
	newLinstorNode := lclient.Node{
		Name: selectedKubernetesNode.Name,
		Type: LinstorSatelliteType,
		NetInterfaces: []lclient.NetInterface{
			{
				Name:                    "default",
				Address:                 net.ParseIP(selectedKubernetesNode.Status.Addresses[0].Address),
				IsActive:                true,
				SatellitePort:           LinstorNodePort,
				SatelliteEncryptionType: LinstorEncryptionType,
			},
		},
		Props: drbdNodeProperties,
	}
	err := lc.Nodes.Create(ctx, newLinstorNode)
	return err
}

func KubernetesNodeLabelsToProperties(kubernetesNodeLabels map[string]string) map[string]string {
	properties := map[string]string{
		"Aux/registered-by": LinstorNodeControllerName,
	}

	for k, v := range kubernetesNodeLabels {
		properties[fmt.Sprintf("Aux/%s", k)] = v
	}

	return properties
}

func GetKubernetesSecretByName(ctx context.Context, cl client.Client, secretName string, secretNamespace string) (*v1.Secret, error) {
	secret := &v1.Secret{}
	err := cl.Get(ctx, client.ObjectKey{
		Name:      secretName,
		Namespace: secretNamespace,
	}, secret)
	return secret, err
}

func GetKubernetesNodesBySelector(ctx context.Context, cl client.Client, nodeSelector map[string]string) (v1.NodeList, error) {
	selectedK8sNodes := v1.NodeList{}
	err := cl.List(ctx, &selectedK8sNodes, client.MatchingLabels(nodeSelector))
	return selectedK8sNodes, err
}

func GetAllKubernetesNodes(ctx context.Context, cl client.Client) (v1.NodeList, error) {
	allKubernetesNodes := v1.NodeList{}
	err := cl.List(ctx, &allKubernetesNodes)
	return allKubernetesNodes, err
}

func GetNodeSelectorFromConfig(secret v1.Secret) (map[string]string, error) {
	var secretConfig sdsapi.SdsDRBDOperatorConfig
	err := yaml.Unmarshal(secret.Data["config"], &secretConfig)
	if err != nil {
		return nil, err
	}
	nodeSelector := secretConfig.NodeSelector
	return nodeSelector, err
}

func DiffNodeLists(leftList, rightList v1.NodeList) v1.NodeList {
	var diff v1.NodeList

	for _, leftNode := range leftList.Items {
		if !ContainsNode(rightList, leftNode) {
			diff.Items = append(diff.Items, leftNode)
		}
	}
	return diff
}

func ContainsNode(nodeList v1.NodeList, node v1.Node) bool {
	for _, item := range nodeList.Items {
		if item.Name == node.Name {
			return true
		}
	}
	return false

}

func GetLinstorNodes(ctx context.Context, lc *lclient.Client) ([]lclient.Node, []lclient.Node, error) {
	linstorNodes, err := lc.Nodes.GetAll(ctx, &lclient.ListOpts{})
	if err != nil {
		return nil, nil, err
	}

	linstorControllerNodes := []lclient.Node{}
	linstorSatelliteNodes := []lclient.Node{}

	for _, linstorNode := range linstorNodes {
		if linstorNode.Type == LinstorControllerType {
			linstorControllerNodes = append(linstorControllerNodes, linstorNode)
		} else if linstorNode.Type == LinstorSatelliteType {
			linstorSatelliteNodes = append(linstorSatelliteNodes, linstorNode)
		}
	}

	return linstorSatelliteNodes, linstorControllerNodes, nil
}

func removeLinstorControllerNodes(ctx context.Context, lc *lclient.Client, log logr.Logger, linstorControllerNodes []lclient.Node) error {
	for _, linstorControllerNode := range linstorControllerNodes {
		log.Info("removeLinstorControllerNodes: Remove LINSTOR controller node: " + linstorControllerNode.Name)
		err := lc.Nodes.Delete(ctx, linstorControllerNode.Name)
		if err != nil {
			return err
		}
	}
	return nil
}
