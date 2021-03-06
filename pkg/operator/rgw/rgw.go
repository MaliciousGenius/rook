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
package rgw

import (
	"fmt"

	"github.com/rook/rook/pkg/cephmgr/client"
	"github.com/rook/rook/pkg/cephmgr/mon"
	cephrgw "github.com/rook/rook/pkg/cephmgr/rgw"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/operator/k8sutil"
	k8smon "github.com/rook/rook/pkg/operator/mon"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/errors"
	"k8s.io/client-go/pkg/api/v1"
	extensions "k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/pkg/util/intstr"
)

const (
	appName     = "rgw"
	keyringName = "keyring"
)

type Cluster struct {
	Namespace string
	Version   string
	Replicas  int32
	factory   client.ConnectionFactory
	dataDir   string
}

func New(namespace, version string, factory client.ConnectionFactory) *Cluster {
	return &Cluster{
		Namespace: namespace,
		Version:   version,
		Replicas:  2,
		factory:   factory,
		dataDir:   k8sutil.DataDir,
	}
}

func (c *Cluster) Start(clientset kubernetes.Interface, cluster *mon.ClusterInfo) error {
	logger.Infof("start running rgw")

	if cluster == nil || len(cluster.Monitors) == 0 {
		return fmt.Errorf("missing mons to start rgw")
	}

	err := c.createKeyring(clientset, cluster)
	if err != nil {
		return fmt.Errorf("failed to create rgw keyring. %+v", err)
	}

	// start the service
	err = c.startService(clientset, cluster)
	if err != nil {
		return fmt.Errorf("failed to start rgw service. %+v", err)
	}

	// start the deployment
	deployment := c.makeDeployment(cluster)
	_, err = clientset.ExtensionsV1beta1().Deployments(c.Namespace).Create(deployment)
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create rgw deployment. %+v", err)
		}
		logger.Infof("rgw deployment already exists")
	} else {
		logger.Infof("rgw deployment started")
	}

	return nil
}

func (c *Cluster) createKeyring(clientset kubernetes.Interface, cluster *mon.ClusterInfo) error {
	_, err := clientset.CoreV1().Secrets(c.Namespace).Get(appName)
	if err == nil {
		logger.Infof("the rgw keyring was already generated")
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to get rgw secrets. %+v", err)
	}

	// connect to the ceph cluster
	logger.Infof("generating rgw keyring")
	context := &clusterd.Context{ConfigDir: c.dataDir}
	conn, err := mon.ConnectToClusterAsAdmin(context, c.factory, cluster)
	if err != nil {
		return fmt.Errorf("failed to connect to cluster. %+v", err)
	}
	defer conn.Shutdown()

	// create the keyring
	keyring, err := cephrgw.CreateKeyring(conn)
	if err != nil {
		return fmt.Errorf("failed to create keyring. %+v", err)
	}

	// store the secrets
	secrets := map[string]string{
		keyringName: keyring,
	}
	secret := &v1.Secret{
		ObjectMeta: v1.ObjectMeta{Name: appName, Namespace: c.Namespace},
		StringData: secrets,
		Type:       k8sutil.RookType,
	}
	_, err = clientset.CoreV1().Secrets(c.Namespace).Create(secret)
	if err != nil {
		return fmt.Errorf("failed to save rgw secrets. %+v", err)
	}

	return nil
}

func (c *Cluster) makeDeployment(cluster *mon.ClusterInfo) *extensions.Deployment {
	deployment := &extensions.Deployment{}
	deployment.Name = appName
	deployment.Namespace = c.Namespace

	podSpec := v1.PodTemplateSpec{
		ObjectMeta: v1.ObjectMeta{
			Name:        "rook-rgw",
			Labels:      getLabels(cluster.Name),
			Annotations: map[string]string{},
		},
		Spec: v1.PodSpec{
			Containers:    []v1.Container{c.rgwContainer(cluster)},
			RestartPolicy: v1.RestartPolicyAlways,
			Volumes: []v1.Volume{
				{Name: k8sutil.DataDirVolume, VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
			},
		},
	}

	deployment.Spec = extensions.DeploymentSpec{Template: podSpec, Replicas: &c.Replicas}

	return deployment
}

func (c *Cluster) rgwContainer(cluster *mon.ClusterInfo) v1.Container {

	command := fmt.Sprintf("/usr/bin/rookd rgw --data-dir=%s --mon-endpoints=%s --cluster-name=%s --rgw-port=%d --rgw-host=%s",
		k8sutil.DataDir, mon.FlattenMonEndpoints(cluster.Monitors), cluster.Name, cephrgw.RGWPort, cephrgw.DNSName)
	return v1.Container{
		// TODO: fix "sleep 5".
		// Without waiting some time, there is highly probable flakes in network setup.
		Command: []string{"/bin/sh", "-c", fmt.Sprintf("sleep 5; %s", command)},
		Name:    appName,
		Image:   k8sutil.MakeRookImage(c.Version),
		VolumeMounts: []v1.VolumeMount{
			{Name: k8sutil.DataDirVolume, MountPath: k8sutil.DataDir},
		},
		Env: []v1.EnvVar{
			{Name: "ROOKD_RGW_KEYRING", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{Name: appName}, Key: keyringName}}},
			k8smon.MonSecretEnvVar(),
			k8smon.AdminSecretEnvVar(),
		},
	}
}

func (c *Cluster) startService(clientset kubernetes.Interface, clusterInfo *mon.ClusterInfo) error {
	labels := getLabels(clusterInfo.Name)
	s := &v1.Service{
		ObjectMeta: v1.ObjectMeta{
			Name:      appName,
			Namespace: c.Namespace,
			Labels:    labels,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:       appName,
					Port:       cephrgw.RGWPort,
					TargetPort: intstr.FromInt(int(cephrgw.RGWPort)),
					Protocol:   v1.ProtocolTCP,
				},
			},
			Selector: labels,
		},
	}

	s, err := clientset.CoreV1().Services(c.Namespace).Create(s)
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create mon service. %+v", err)
		}
		logger.Infof("RGW service already running")
		return nil
	}

	logger.Infof("RGW service running at %s:%d", s.Spec.ClusterIP, cephrgw.RGWPort)
	return nil
}

func getLabels(clusterName string) map[string]string {
	return map[string]string{
		k8sutil.AppAttr:     appName,
		k8sutil.ClusterAttr: clusterName,
	}
}
