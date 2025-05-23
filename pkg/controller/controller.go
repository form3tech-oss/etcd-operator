// Copyright 2016 The etcd-operator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	api "github.com/coreos/etcd-operator/pkg/apis/etcd/v1beta2"
	"github.com/coreos/etcd-operator/pkg/cluster"
	"github.com/coreos/etcd-operator/pkg/generated/clientset/versioned"
	"github.com/coreos/etcd-operator/pkg/util/k8sutil"

	"github.com/sirupsen/logrus"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kwatch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

var (
	initRetryWaitTime      = 30 * time.Second
	terminationGracePeriod = int64(5)
)

type Event struct {
	Type   kwatch.EventType
	Object *api.EtcdCluster
}

type Controller struct {
	logger *logrus.Entry
	Config

	clusters map[string]*cluster.Cluster
}

type Config struct {
	Namespace         string
	ClusterWide       bool
	ServiceAccount    string
	KubeCli           kubernetes.Interface
	KubeExtCli        apiextensionsclient.Interface
	EtcdCRCli         versioned.Interface
	CreateCRD         bool
	RecoverQuorumLoss bool
}

func New(cfg Config) *Controller {
	return &Controller{
		logger: logrus.WithField("pkg", "controller"),

		Config:   cfg,
		clusters: make(map[string]*cluster.Cluster),
	}
}

// handleClusterEvent returns true if cluster is ignored (not managed) by this instance.
func (c *Controller) handleClusterEvent(event *Event) (bool, error) {
	clus := event.Object

	if !c.managed(clus) {
		return true, nil
	}

	if clus.Status.IsFailed() {
		// delete failed cluster
		// the update event will re-create the cluster afterwards
		if clus.Spec.FailurePolicy == api.FailurePolicyRecreate {
			c.logger.Infof("deleting cluster due to failurePolicy=Recreate")
			inst, ok := c.clusters[getNamespacedName(clus)]
			if ok {
				inst.Delete()
				clustersTotal.Dec()
			}
			delete(c.clusters, getNamespacedName(clus))

			// operator does not clean up resources on it's own, it relies
			// on ownerReferences / k8s gc to cleanup orphaned pods
			// for this case we want to clean up manually
			c.logger.Info("cleaning up cluster resources")
			err := c.cleanupClusterResources(clus)
			if err != nil {
				c.logger.Errorf("unable to cleanup cluster resources: %v", err)
			}

			// reset cluster status
			clus.Status = api.ClusterStatus{}
			_, err = c.EtcdCRCli.EtcdV1beta2().EtcdClusters(clus.Namespace).Update(context.Background(), clus, v1.UpdateOptions{})
			if err != nil {
				c.logger.Error(err)
				return false, err
			}
			return false, nil
		}

		clustersFailed.Inc()
		if event.Type == kwatch.Deleted {
			delete(c.clusters, getNamespacedName(clus))
			return false, nil
		}
		return false, fmt.Errorf("ignore failed cluster (%s). Please delete its CR", clus.Name)
	}

	clus.SetDefaults()

	if err := clus.Spec.Validate(); err != nil {
		return false, fmt.Errorf("invalid cluster spec. please fix the following problem with the cluster spec: %v", err)
	}

	switch event.Type {
	case kwatch.Added:
		if _, ok := c.clusters[getNamespacedName(clus)]; ok {
			return false, fmt.Errorf("unsafe state. cluster (%s) was created before but we received event (%s)", clus.Name, event.Type)
		}

		nc := cluster.New(c.makeClusterConfig(), clus)
		if nc == nil {
			return false, fmt.Errorf("cluster name cannot be more than %v characters long, please delete the CR", k8sutil.MaxNameLength)
		}
		c.clusters[getNamespacedName(clus)] = nc

		clustersCreated.Inc()
		clustersTotal.Inc()

	case kwatch.Modified:
		if _, ok := c.clusters[getNamespacedName(clus)]; !ok {
			return false, fmt.Errorf("unsafe state. cluster (%s) was never created but we received event (%s)", clus.Name, event.Type)
		}
		c.clusters[getNamespacedName(clus)].Update(clus)
		clustersModified.Inc()

	case kwatch.Deleted:
		if _, ok := c.clusters[getNamespacedName(clus)]; !ok {
			return false, fmt.Errorf("unsafe state. cluster (%s) was never created but we received event (%s)", clus.Name, event.Type)
		}
		c.clusters[getNamespacedName(clus)].Delete()
		delete(c.clusters, getNamespacedName(clus))
		clustersDeleted.Inc()
		clustersTotal.Dec()
	}
	return false, nil
}

func (c *Controller) cleanupClusterResources(clus *api.EtcdCluster) error {
	var errs []string
	// pod disruption budget
	err := c.KubeCli.PolicyV1().PodDisruptionBudgets(clus.Namespace).Delete(context.Background(), clus.Name, *v1.NewDeleteOptions(terminationGracePeriod))
	if err != nil {
		errs = append(errs, err.Error())
	}
	// etcd cluster member pods
	err = c.KubeCli.CoreV1().Pods(clus.Namespace).DeleteCollection(context.Background(), *v1.NewDeleteOptions(terminationGracePeriod), v1.ListOptions{
		LabelSelector: "etcd_cluster=" + clus.Name,
	})
	if err != nil {
		errs = append(errs, err.Error())
	}
	// peer service
	err = c.KubeCli.CoreV1().Services(clus.Namespace).Delete(context.Background(), clus.Name, *v1.NewDeleteOptions(terminationGracePeriod))
	if err != nil {
		errs = append(errs, err.Error())
	}
	// client service
	err = c.KubeCli.CoreV1().Services(clus.Namespace).Delete(context.Background(), k8sutil.ClientServiceName(clus.Name), *v1.NewDeleteOptions(terminationGracePeriod))
	if err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, ", "))
	}
	return nil
}

func (c *Controller) makeClusterConfig() cluster.Config {
	return cluster.Config{
		ServiceAccount:    c.Config.ServiceAccount,
		KubeCli:           c.Config.KubeCli,
		EtcdCRCli:         c.Config.EtcdCRCli,
		RecoverQuorumLoss: c.Config.RecoverQuorumLoss,
	}
}

func (c *Controller) initCRD() error {
	err := k8sutil.CreateCRD(c.KubeExtCli, api.EtcdClusterCRDName, api.EtcdClusterResourceKind, api.EtcdClusterResourcePlural, "etcd")
	if err != nil {
		return fmt.Errorf("failed to create CRD: %v", err)
	}
	return k8sutil.WaitCRDReady(c.KubeExtCli, api.EtcdClusterCRDName)
}

func getNamespacedName(c *api.EtcdCluster) string {
	return fmt.Sprintf("%s%c%s", c.Namespace, '/', c.Name)
}
