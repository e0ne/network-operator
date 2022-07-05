/*
Copyright 2020 NVIDIA

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

package state

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/source"

	mellanoxv1alpha1 "github.com/Mellanox/network-operator/api/v1alpha1"
	"github.com/Mellanox/network-operator/pkg/config"
	"github.com/Mellanox/network-operator/pkg/consts"
	"github.com/Mellanox/network-operator/pkg/nodeinfo"
	"github.com/Mellanox/network-operator/pkg/render"
	"github.com/Mellanox/network-operator/pkg/utils"
)

const stateOFEDName = "state-OFED"
const stateOFEDDescription = "OFED driver deployed in the cluster"

//nolint:lll
// CertConfigPathMap indicates standard OS specific paths for ssl keys/certificates.
// Where Go looks for certs: https://golang.org/src/crypto/x509/root_linux.go
// Where OCP mounts proxy certs on RHCOS nodes:
// https://access.redhat.com/documentation/en-us/openshift_container_platform/4.3/html/authentication/ocp-certificates#proxy-certificates_ocp-certificates
var CertConfigPathMap = map[string]string{
	"ubuntu": "/etc/ssl/certs",
	"rhcos":  "/etc/pki/ca-trust/extracted/pem",
}

// RepoConfigPathMap indicates standard OS specific paths for repository configuration files
var RepoConfigPathMap = map[string]string{
	"ubuntu": "/etc/apt/sources.list.d",
	"rhcos":  "/etc/yum.repos.d",
}

// NewStateOFED creates a new OFED driver state
func NewStateOFED(k8sAPIClient client.Client, scheme *runtime.Scheme, manifestDir string) (State, error) {
	files, err := utils.GetFilesWithSuffix(manifestDir, render.ManifestFileSuffix...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get files from manifest dir")
	}

	renderer := render.NewRenderer(files)
	return &stateOFED{
		stateSkel: stateSkel{
			name:        stateOFEDName,
			description: stateOFEDDescription,
			client:      k8sAPIClient,
			scheme:      scheme,
			renderer:    renderer,
		}}, nil
}

type stateOFED struct {
	stateSkel
}

type additionalVolumeMounts struct {
	VolumeMounts []v1.VolumeMount
	Volumes      []v1.Volume
}

type ofedRuntimeSpec struct {
	runtimeSpec
	CPUArch string
	OSName  string
	OSVer   string
}

type ofedManifestRenderData struct {
	CrSpec                 *mellanoxv1alpha1.OFEDDriverSpec
	NodeAffinity           *v1.NodeAffinity
	RuntimeSpec            *ofedRuntimeSpec
	AdditionalVolumeMounts additionalVolumeMounts
}

// getCertConfigPath returns the standard OS specific path for ssl keys/certificates
func getCertConfigPath(osname string) (string, error) {
	if path, ok := CertConfigPathMap[osname]; ok {
		return path, nil
	}
	return "", fmt.Errorf("distribution not supported")
}

// getRepoConfigPath returns the standard OS specific path for repository configuration files
func getRepoConfigPath(osname string) (string, error) {
	if path, ok := RepoConfigPathMap[osname]; ok {
		return path, nil
	}
	return "", fmt.Errorf("distribution not supported")
}

// createConfigMapVolumeMounts creates a VolumeMount for each key
// in the ConfigMap. Use subPath to ensure original contents
// at destinationDir are not overwritten.
func (s *stateOFED) createConfigMapVolumeMounts(namespace, configMapName, destinationDir string) (
	[]v1.VolumeMount, []v1.KeyToPath, error) {
	// get the ConfigMap
	cm := &v1.ConfigMap{}

	objKey := client.ObjectKey{Namespace: namespace, Name: configMapName}
	err := s.client.Get(context.TODO(), objKey, cm)
	if err != nil {
		return nil, nil, fmt.Errorf("ERROR: could not get ConfigMap %s from client: %v", configMapName, err)
	}

	// create one volume mount per file in the ConfigMap and use subPath
	var filenames = make([]string, 0, len(cm.Data))
	for filename := range cm.Data {
		filenames = append(filenames, filename)
	}
	// sort so volume mounts are added to spec in deterministic order to make testing easier
	sort.Strings(filenames)
	var itemsToInclude = make([]v1.KeyToPath, 0, len(filenames))
	var volumeMounts = make([]v1.VolumeMount, 0, len(filenames))
	for _, filename := range filenames {
		volumeMounts = append(volumeMounts,
			v1.VolumeMount{
				Name:      configMapName,
				ReadOnly:  true,
				MountPath: filepath.Join(destinationDir, filename),
				SubPath:   filename})
		itemsToInclude = append(itemsToInclude, v1.KeyToPath{
			Key:  filename,
			Path: filename,
		})
	}
	return volumeMounts, itemsToInclude, nil
}

func (s *stateOFED) createConfigMapVolume(configMapName string, itemsToInclude []v1.KeyToPath) v1.Volume {
	volumeSource := v1.VolumeSource{
		ConfigMap: &v1.ConfigMapVolumeSource{
			LocalObjectReference: v1.LocalObjectReference{
				Name: configMapName,
			},
			Items: itemsToInclude,
		},
	}
	return v1.Volume{Name: configMapName, VolumeSource: volumeSource}
}

// Sync attempt to get the system to match the desired state which State represent.
// a sync operation must be relatively short and must not block the execution thread.
//nolint:dupl
func (s *stateOFED) Sync(customResource interface{}, infoCatalog InfoCatalog) (SyncState, error) {
	cr := customResource.(*mellanoxv1alpha1.NicClusterPolicy)
	log.V(consts.LogLevelInfo).Info(
		"Sync Custom resource", "State:", s.name, "Name:", cr.Name, "Namespace:", cr.Namespace)

	if cr.Spec.OFEDDriver == nil {
		// Either this state was not required to run or an update occurred and we need to remove
		// the resources that where created.
		// TODO: Support the latter case
		log.V(consts.LogLevelInfo).Info("OFED driver spec in CR is nil, no action required")
		return SyncStateIgnore, nil
	}
	// Fill ManifestRenderData and render objects
	nodeInfo := infoCatalog.GetNodeInfoProvider()
	if nodeInfo == nil {
		return SyncStateError, errors.New("unexpected state, catalog does not provide node information")
	}

	objs, err := s.getManifestObjects(cr, nodeInfo)
	if err != nil {
		return SyncStateNotReady, errors.Wrap(err, "failed to create k8s objects from manifest")
	}
	if len(objs) == 0 {
		// getManifestObjects returned no objects, this means that no objects need to be applied to the cluster
		// as (most likely) no Mellanox hardware is found (No mellanox labels where found).
		// Return SyncStateNotReady so we retry the Sync.
		return SyncStateNotReady, nil
	}

	// Create objects if they dont exist, Update objects if they do exist
	err = s.createOrUpdateObjs(func(obj *unstructured.Unstructured) error {
		if err := controllerutil.SetControllerReference(cr, obj, s.scheme); err != nil {
			return errors.Wrap(err, "failed to set controller reference for object")
		}
		return nil
	}, objs)
	if err != nil {
		return SyncStateNotReady, errors.Wrap(err, "failed to create/update objects")
	}
	// Check objects status
	syncState, err := s.getSyncState(objs)
	if err != nil {
		return SyncStateNotReady, errors.Wrap(err, "failed to get sync state")
	}
	return syncState, nil
}

// Get a map of source kinds that should be watched for the state keyed by the source kind name
func (s *stateOFED) GetWatchSources() map[string]*source.Kind {
	wr := make(map[string]*source.Kind)
	wr["DaemonSet"] = &source.Kind{Type: &appsv1.DaemonSet{}}
	return wr
}

func (s *stateOFED) mountAdditionalVolumesFromConfigMap(volMounts *additionalVolumeMounts, configMapName, destDir string) error {
	volumeMounts, itemsToInclude, err := s.createConfigMapVolumeMounts(
		config.FromEnv().State.NetworkOperatorResourceNamespace,
		configMapName,
		destDir)
	if err != nil {
		return fmt.Errorf("ERROR: failed to create ConfigMap VolumeMounts for custom repo config: %v", err)
	}
	volume := s.createConfigMapVolume(configMapName, itemsToInclude)
	volMounts.VolumeMounts = append(volMounts.VolumeMounts, volumeMounts...)
	volMounts.Volumes = append(volMounts.Volumes, volume)

	return nil
}

func (s *stateOFED) getManifestObjects(
	cr *mellanoxv1alpha1.NicClusterPolicy,
	nodeInfo nodeinfo.Provider) ([]*unstructured.Unstructured, error) {
	attrs := nodeInfo.GetNodesAttributes(
		nodeinfo.NewNodeLabelFilterBuilder().WithLabel(nodeinfo.NodeLabelMlnxNIC, "true").Build())
	if len(attrs) == 0 {
		log.V(consts.LogLevelInfo).Info("No nodes with Mellanox NICs where found in the cluster.")
		return []*unstructured.Unstructured{}, nil
	}

	// TODO: Render daemonset multiple times according to CPUXOS matrix (ATM assume all nodes are the same)
	// Note: it is assumed MOFED driver container is able to handle multiple kernel version e.g by triggering DKMS
	// if driver was compiled against a missmatching kernel to begin with.
	if err := s.checkAttributesExist(attrs[0],
		nodeinfo.AttrTypeCPUArch, nodeinfo.AttrTypeOSName, nodeinfo.AttrTypeOSVer); err != nil {
		return nil, err
	}

	if cr.Spec.OFEDDriver.StartupProbe == nil {
		cr.Spec.OFEDDriver.StartupProbe = &mellanoxv1alpha1.PodProbeSpec{
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
		}
	}

	if cr.Spec.OFEDDriver.LivenessProbe == nil {
		cr.Spec.OFEDDriver.LivenessProbe = &mellanoxv1alpha1.PodProbeSpec{
			InitialDelaySeconds: 30,
			PeriodSeconds:       30,
		}
	}

	if cr.Spec.OFEDDriver.ReadinessProbe == nil {
		cr.Spec.OFEDDriver.ReadinessProbe = &mellanoxv1alpha1.PodProbeSpec{
			InitialDelaySeconds: 10,
			PeriodSeconds:       30,
		}
	}

	additionalVolMounts := additionalVolumeMounts{}
	osname := attrs[0].Attributes[nodeinfo.AttrTypeOSName]
	// set any custom ssl key/certificate configuration provided
	if cr.Spec.OFEDDriver.CertConfig != nil && cr.Spec.OFEDDriver.CertConfig.Name != "" {
		destinationDir, err := getCertConfigPath(osname)
		if err != nil {
			return nil, fmt.Errorf("failed to get destination directory for custom repo config: %v", err)
		}

		err = s.mountAdditionalVolumesFromConfigMap(&additionalVolMounts, cr.Spec.OFEDDriver.CertConfig.Name, destinationDir)
		if err != nil {
			return nil, fmt.Errorf("failed to custom certificates: %v", err)
		}
	}

	// set any custom repo configuration provided
	if cr.Spec.OFEDDriver.RepoConfig != nil && cr.Spec.OFEDDriver.RepoConfig.Name != "" {
		destinationDir, err := getRepoConfigPath(osname)
		if err != nil {
			return nil, fmt.Errorf("ERROR: failed to get destination directory for custom repo config: %v", err)
		}

		err = s.mountAdditionalVolumesFromConfigMap(&additionalVolMounts, cr.Spec.OFEDDriver.RepoConfig.Name, destinationDir)
		if err != nil {
			return nil, fmt.Errorf("failed to custom repositories configuration: %v", err)
		}
	}

	renderData := &ofedManifestRenderData{
		CrSpec: cr.Spec.OFEDDriver,
		RuntimeSpec: &ofedRuntimeSpec{
			runtimeSpec: runtimeSpec{config.FromEnv().State.NetworkOperatorResourceNamespace},
			CPUArch:     attrs[0].Attributes[nodeinfo.AttrTypeCPUArch],
			OSName:      attrs[0].Attributes[nodeinfo.AttrTypeOSName],
			OSVer:       attrs[0].Attributes[nodeinfo.AttrTypeOSVer],
		},
		NodeAffinity:           cr.Spec.NodeAffinity,
		AdditionalVolumeMounts: additionalVolMounts,
	}
	// render objects
	log.V(consts.LogLevelDebug).Info("Rendering objects", "data:", renderData)
	objs, err := s.renderer.RenderObjects(&render.TemplatingData{Data: renderData})
	if err != nil {
		return nil, errors.Wrap(err, "failed to render objects")
	}
	log.V(consts.LogLevelDebug).Info("Rendered", "objects:", objs)
	return objs, nil
}
