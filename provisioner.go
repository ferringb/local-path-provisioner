package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	pvController "sigs.k8s.io/sig-storage-lib-external-provisioner/v8/controller"
)

type ActionType string

const (
	ActionTypeCreate = "create"
	ActionTypeDelete = "delete"
)

const (
	KeyNode = "kubernetes.io/hostname"

	helperScriptDir     = "/script"
	helperDataVolName   = "data"
	helperScriptVolName = "script"

	envVolDir  = "VOL_DIR"
	envVolMode = "VOL_MODE"
	envVolSize = "VOL_SIZE_BYTES"
)

var (
	HelperPodNameMaxLength = 128
)

type LocalPathProvisioner struct {
	kubeClient *clientset.Clientset
	namespace  string

	config        *Config
	configMapName string
	configMutex   *sync.RWMutex
}

func NewProvisioner(ctx context.Context, kubeClient *clientset.Clientset,
	config *Config, namespace string, configMapName string) (result *LocalPathProvisioner, err error) {
	p := &LocalPathProvisioner{
		kubeClient:    kubeClient,
		config:        config,
		namespace:     namespace,
		configMapName: configMapName,
		configMutex:   &sync.RWMutex{},
	}
	return p, nil
}

func (p *LocalPathProvisioner) Provision(ctx context.Context, opts pvController.ProvisionOptions) (*v1.PersistentVolume, pvController.ProvisioningState, error) {
	p.configMutex.RLock()
	defer p.configMutex.RUnlock()
	pvc := opts.PVC
	if pvc.Spec.Selector != nil {
		return nil, pvController.ProvisioningFinished, fmt.Errorf("claim.Spec.Selector is not supported")
	}
	for _, accessMode := range pvc.Spec.AccessModes {
		if accessMode != v1.ReadWriteOnce {
			return nil, pvController.ProvisioningFinished, fmt.Errorf("Only support ReadWriteOnce access mode")
		}
	}
	node := opts.SelectedNode
	if opts.SelectedNode == nil {
		return nil, pvController.ProvisioningFinished, fmt.Errorf("configuration error, no node was specified")
	}

	nodeConfig := p.config.GetNodeConfig(node.Name)

	name := opts.PVName
	folderName := strings.Join([]string{name, opts.PVC.Namespace, opts.PVC.Name}, "_")

	path := filepath.Join(nodeConfig.Path, folderName)
	logrus.Infof("Creating volume %v at %v:%v", name, node.Name, path)

	storage := pvc.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)]
	if err := p.createHelperPod(ctx, ActionTypeCreate, nodeConfig, volumeOptions{
		Name:        name,
		Path:        path,
		Mode:        *pvc.Spec.VolumeMode,
		SizeInBytes: storage.Value(),
		Node:        node.Name,
	}); err != nil {
		return nil, pvController.ProvisioningFinished, err
	}

	fs := v1.PersistentVolumeFilesystem
	hostPathType := v1.HostPathDirectoryOrCreate

	valueNode, ok := node.GetLabels()[KeyNode]
	if !ok {
		valueNode = node.Name
	}

	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *opts.StorageClass.ReclaimPolicy,
			AccessModes:                   pvc.Spec.AccessModes,
			VolumeMode:                    &fs,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): pvc.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)],
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: path,
					Type: &hostPathType,
				},
			},
			NodeAffinity: &v1.VolumeNodeAffinity{
				Required: &v1.NodeSelector{
					NodeSelectorTerms: []v1.NodeSelectorTerm{
						{
							MatchExpressions: []v1.NodeSelectorRequirement{
								{
									Key:      KeyNode,
									Operator: v1.NodeSelectorOpIn,
									Values: []string{
										valueNode,
									},
								},
							},
						},
					},
				},
			},
		},
	}, pvController.ProvisioningFinished, nil
}

func (p *LocalPathProvisioner) Delete(ctx context.Context, pv *v1.PersistentVolume) (err error) {
	p.configMutex.RLock()
	defer p.configMutex.RUnlock()
	defer func() {
		err = errors.Wrapf(err, "failed to delete volume %v", pv.Name)
	}()
	path, nodeName, err := p.getPathAndNodeFromPV(pv)
	if err != nil {
		return err
	}
	nodeConfig := p.config.GetNodeConfig(nodeName)

	if pv.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimRetain {
		logrus.Infof("Deleting volume %v at %v:%v", pv.Name, nodeName, path)
		storage := pv.Spec.Capacity[v1.ResourceName(v1.ResourceStorage)]
		if err := p.createHelperPod(ctx, ActionTypeDelete, nodeConfig, volumeOptions{
			Name:        pv.Name,
			Path:        path,
			Mode:        *pv.Spec.VolumeMode,
			SizeInBytes: storage.Value(),
			Node:        nodeName,
		}); err != nil {
			logrus.Infof("clean up volume %v failed: %v", pv.Name, err)
			return err
		}
		return nil
	}
	logrus.Infof("Retained volume %v", pv.Name)
	return nil
}

func (p *LocalPathProvisioner) getPathAndNodeFromPV(pv *v1.PersistentVolume) (string, string, error) {
	hostPath := pv.Spec.PersistentVolumeSource.HostPath
	if hostPath == nil {
		return "", "", fmt.Errorf("no HostPath set")
	}

	nodeAffinity := pv.Spec.NodeAffinity
	if nodeAffinity == nil {
		return "", "", fmt.Errorf("no NodeAffinity set")
	}
	required := nodeAffinity.Required
	if required == nil {
		return "", "", fmt.Errorf("no NodeAffinity.Required set")
	}

	for _, selectorTerm := range required.NodeSelectorTerms {
		for _, expression := range selectorTerm.MatchExpressions {
			if expression.Key == KeyNode && expression.Operator == v1.NodeSelectorOpIn {
				if len(expression.Values) != 1 {
					return "", "", fmt.Errorf("multiple values for the node affinity")
				}
				return hostPath.Path, expression.Values[0], nil
			}
		}
	}
	return "", "", fmt.Errorf("cannot find affinited node")
}

type volumeOptions struct {
	Name        string
	Path        string
	Mode        v1.PersistentVolumeMode
	SizeInBytes int64
	Node        string
}

func (p *LocalPathProvisioner) createHelperPod(ctx context.Context, action ActionType, nodeConfig *NodeConfig, o volumeOptions) (err error) {
	defer func() {
		err = errors.Wrapf(err, "failed to %v volume %v", action, o.Name)
	}()
	if o.Name == "" || o.Path == "" || o.Node == "" {
		return fmt.Errorf("invalid empty name or path or node")
	}
	if !filepath.IsAbs(o.Path) {
		return fmt.Errorf("volume path %s is not absolute", o.Path)
	}
	o.Path = filepath.Clean(o.Path)
	parentDir, volumeDir := filepath.Split(o.Path)
	hostPathType := v1.HostPathDirectoryOrCreate
	lpvVolumes := []v1.Volume{
		{
			Name: helperDataVolName,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: parentDir,
					Type: &hostPathType,
				},
			},
		},
		{
			Name: helperScriptVolName,
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{
						Name: p.configMapName,
					},
					Items: []v1.KeyToPath{
						{
							Key:  "setup",
							Path: "setup",
						},
						{
							Key:  "teardown",
							Path: "teardown",
						},
					},
				},
			},
		},
	}
	lpvTolerations := []v1.Toleration{
		{
			Operator: v1.TolerationOpExists,
		},
	}
	helperPod := nodeConfig.GetHelperPod()

	scriptMount := addVolumeMount(&helperPod.Spec.Containers[0].VolumeMounts, helperScriptVolName, helperScriptDir)
	scriptMount.MountPath = helperScriptDir
	dataMount := addVolumeMount(&helperPod.Spec.Containers[0].VolumeMounts, helperDataVolName, parentDir)
	parentDir = dataMount.MountPath
	parentDir = strings.TrimSuffix(parentDir, string(filepath.Separator))
	volumeDir = strings.TrimSuffix(volumeDir, string(filepath.Separator))
	if parentDir == "" || volumeDir == "" || !filepath.IsAbs(parentDir) {
		// it covers the `/` case
		return fmt.Errorf("invalid path %v for %v: cannot find parent dir or volume dir or parent dir is relative", action, o.Path)
	}
	env := []v1.EnvVar{
		{Name: envVolDir, Value: filepath.Join(parentDir, volumeDir)},
		{Name: envVolMode, Value: string(o.Mode)},
		{Name: envVolSize, Value: strconv.FormatInt(o.SizeInBytes, 10)},
	}

	// use different name for helper pods
	// https://github.com/rancher/local-path-provisioner/issues/154
	helperPod.Name = (helperPod.Name + "-" + string(action) + "-" + o.Name)
	if len(helperPod.Name) > HelperPodNameMaxLength {
		helperPod.Name = helperPod.Name[:HelperPodNameMaxLength]
	}
	// disable K8s api access unless the podspec has an explicit service account.
	if helperPod.Spec.ServiceAccountName == "" && helperPod.Spec.DeprecatedServiceAccount == "" {
		logrus.Debug("disabling serviceAccount token mounting since no service account is set")
		b := false
		helperPod.Spec.AutomountServiceAccountToken = &b
	}
	helperPod.Namespace = p.namespace
	helperPod.Spec.NodeName = o.Node
	helperPod.Spec.RestartPolicy = v1.RestartPolicyNever
	helperPod.Spec.Tolerations = append(helperPod.Spec.Tolerations, lpvTolerations...)
	helperPod.Spec.Volumes = append(helperPod.Spec.Volumes, lpvVolumes...)

	cmd := []string{"/bin/sh"}
	var cmdTimeout time.Duration
	switch action {
	case ActionTypeCreate:
		cmd = append(cmd, "/script/setup")
		cmdTimeout = nodeConfig.SetupTimeout
	case ActionTypeDelete:
		cmd = append(cmd, "/script/teardown")
		cmdTimeout = nodeConfig.TeardownTimeout
	default:
		return fmt.Errorf("action type %v is unknown/unsupported", action)
	}

	helperPod.Spec.Containers[0].Command = cmd
	helperPod.Spec.Containers[0].Env = append(helperPod.Spec.Containers[0].Env, env...)
	helperPod.Spec.Containers[0].Args = []string{"-p", filepath.Join(parentDir, volumeDir),
		"-s", strconv.FormatInt(o.SizeInBytes, 10),
		"-m", string(o.Mode)}

	// If it already exists due to some previous errors, the pod will be cleaned up later automatically
	// https://github.com/rancher/local-path-provisioner/issues/27
	logrus.Infof("create the helper pod %s into %s", helperPod.Name, p.namespace)
	_, err = p.kubeClient.CoreV1().Pods(p.namespace).Create(ctx, helperPod, metav1.CreateOptions{})
	if err != nil && !k8serror.IsAlreadyExists(err) {
		return err
	}

	defer func() {
		e := p.kubeClient.CoreV1().Pods(p.namespace).Delete(ctx, helperPod.Name, metav1.DeleteOptions{})
		if e != nil {
			logrus.Errorf("unable to delete the helper pod: %v", e)
		}
	}()

	// replace this loop with a watch.
	completed := false

	for i := 0.0; i < cmdTimeout.Seconds(); i++ {
		if pod, err := p.kubeClient.CoreV1().Pods(p.namespace).Get(ctx, helperPod.Name, metav1.GetOptions{}); err != nil {
			return err
		} else if pod.Status.Phase == v1.PodSucceeded {
			completed = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !completed {
		return fmt.Errorf("create process timeout after %v seconds", cmdTimeout.Seconds())
	}

	logrus.Infof("Volume %v has been %vd on %v:%v", o.Name, action, o.Node, o.Path)
	return nil
}

func addVolumeMount(mounts *[]v1.VolumeMount, name, mountPath string) *v1.VolumeMount {
	for i, m := range *mounts {
		if m.Name == name {
			if m.MountPath == "" {
				(*mounts)[i].MountPath = mountPath
			}
			return &(*mounts)[i]
		}
	}
	*mounts = append(*mounts, v1.VolumeMount{Name: name, MountPath: mountPath})
	return &(*mounts)[len(*mounts)-1]
}
