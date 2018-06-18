// Copyright © 2018 Aviv Laufer <aviv.laufer@gmail.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package compute

import (
	"errors"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	cfg "github.com/doitintl/kip/pkg/config"
	"github.com/doitintl/kip/pkg/types"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
)

var scopes string

/*func init() {
	scopes = strings.Join([]string{
		compute.DevstorageFullControlScope,
		compute.ComputeScope,
	}, " ")

}*/

func ListClusterZones(projectID string, clusterName string) ([]string, error) {
	retval := make([]string, 0)
	ctx := context.Background()
	hc, err := google.DefaultClient(ctx, container.CloudPlatformScope)
	if err != nil {
		logrus.Fatalf("Could not get authenticated client: %v", err)
	}

	svc, err := container.New(hc)
	if err != nil {
		logrus.Fatalf("Could not initialize gke client: %v", err)
		return retval, err
	}
	var zone string = "-"
	list, err := svc.Projects.Zones.Clusters.List(projectID, zone).Do()
	if err != nil {
		logrus.Errorf("failed to list clusters: %v", err)
		return retval, err
	}
	for _, v := range list.Clusters {
		if strings.Compare(strings.ToLower(clusterName), strings.ToLower(v.Name)) == 0 {
			logrus.Infof("Cluster %q (%s) master_version: v%s zone %s", v.Name, v.Status, v.CurrentMasterVersion, v.Locations)
			return v.Locations, nil
		}

	}
	return retval, errors.New("cluster not found")
}

func ClusterName() (string, error) {
	req, err := http.NewRequest("GET", "http://metadata/computeMetadata/v1/instance/attributes/cluster-name", nil)
	if err != nil {
		return "", err
	}
	req.Header.Add("Metadata-Flavor", "Google")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", errors.New("discover-gce: invalid status code 0 when fetching project id")
	}

	cluster, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(cluster), nil
}

func ProjectName() (string, error) {
	req, err := http.NewRequest("GET", "http://metadata.google.internal/computeMetadata/v1/project/project-id", nil)
	if err != nil {
		return "", err
	}
	req.Header.Add("Metadata-Flavor", "Google")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", errors.New("discover-gce: invalid status code when fetching project id")
	}

	project, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(project), nil
}

func FindAddress(projectID string, region string,config *cfg.Config) (string, error) {
	ctx := context.Background()
	hc, err := google.DefaultClient(ctx, container.CloudPlatformScope)
	if err != nil {
		logrus.Error(err)
		return "", err
	}
	computeService, err := compute.New(hc)
	if err != nil {
		logrus.Error(err)
		return "", err
	}
	filter := "(labels." + config.LabelKey + "=" + config.LabelValue + ")"
	addresses, _ := computeService.Addresses.List(projectID, region).Filter("(status=RESERVED) AND " + filter).Do()
	if len(addresses.Items) != 0 {
		return addresses.Items[0].Address, nil
	}
	return "", errors.New("no free address found")

}

func replaceIP(projectID string, zone string, instance string, config *cfg.Config) error {
	ctx := context.Background()
	hc, err := google.DefaultClient(ctx, container.CloudPlatformScope)
	if err != nil {
		logrus.Fatalf("Could not get authenticated client: %v", err)
	}
	region :=zone[:len(zone)-2]
	addr, err := FindAddress(projectID, region, config)
	if err != nil {
		logrus.Infof(err.Error())
		return err
	}
	logrus.Infof("Found reserved address %s", addr)
	computeService, err := compute.New(hc)
	op, err := computeService.Instances.DeleteAccessConfig(projectID, zone, instance, "external-nat", "nic0").Do()
	if err != nil {
		logrus.Errorf("DeleteAccessConfig %q", err)
		return err

	}
	waitForComplition(projectID, zone, op)
	accessConfig := &compute.AccessConfig{
		Name:  "External NAT",
		Type:  "ONE_TO_ONE_NAT",
		NatIP: addr,
		Kind:  "compute#accessConfig",
	}
	op, err = computeService.Instances.AddAccessConfig(projectID, zone, instance, "nic0", accessConfig).Do()
	if err != nil {
		logrus.Errorf("AddAccessConfig %q", err)
		return err
	}
	waitForComplition(projectID, zone, op)
	logrus.Infof("Replaced IP for %s new ip %s", instance, addr)
	return nil

}

func waitForComplition(projectID string, zone string, operation *compute.Operation) (err error) {
	ctx := context.Background()
	hc, err := google.DefaultClient(ctx, container.CloudPlatformScope)
	if err != nil {
		logrus.Fatalf("Could not get authenticated client: %v", err)
	}
	computeService, err := compute.New(hc)
	for {
		op, err := computeService.ZoneOperations.Get(projectID, zone, operation.Name).Do()
		if err != nil {
			logrus.Errorf("ZoneOperations.Get %q %s", err, operation.Name)
			return err
		}
		logrus.Infof("Status %s", op.Status)
		if strings.ToLower(op.Status) != "done" {
			logrus.Info("sleeping")
			time.Sleep(2 * time.Second)
		} else {
			return nil
		}
	}
}
func Kip(instance <-chan types.Instance, config *cfg.Config) {
	for {
		inst := <-instance
		logrus.Infof("Working on %s", inst.Name)
		replaceIP(inst.ProjectID, inst.Zone, inst.Name, config)
		//TODO Wait for completion of ip status


	}

}
