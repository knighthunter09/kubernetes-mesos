package mesos

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"

	log "github.com/golang/glog"
	"github.com/mesosphere/kubernetes-mesos/pkg/config"
	"golang.org/x/net/context"
)

const (
	mesosHttpClientTimeout = 10 * time.Second //TODO(jdef) configurable via fiag?
)

type mesosClient struct {
	mesosMaster string
	client      *http.Client
	tr          *http.Transport
}

func newMesosClient() *mesosClient {
	tr := &http.Transport{}
	return &mesosClient{
		mesosMaster: MasterURI(),
		client: &http.Client{
			Transport: tr,
			Timeout:   mesosHttpClientTimeout,
		},
		tr: tr,
	}
}

// return an array of host:port strings, each of which points to a mesos slave service
func (c *mesosClient) EnumerateSlaves(ctx context.Context) ([]string, error) {
	//TODO(jdef) probably should not assume that mesosMaster is a host:port
	uri := fmt.Sprintf("http://%s/state.json", c.mesosMaster)
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return nil, err
	}
	hosts := []string{}
	err = c.httpDo(ctx, req, func(res *http.Response, err error) error {
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("HTTP request failed with code %d: %v", res.StatusCode, res.Status)
		}
		blob, err1 := ioutil.ReadAll(res.Body)
		if err1 != nil {
			return err1
		}
		log.V(3).Infof("Got mesos state, content length %v", len(blob))
		type State struct {
			Slaves []*struct {
				Id       string `json:"id"`       // ex: 20150106-162714-3815890698-5050-2453-S2
				Pid      string `json:"pid"`      // ex: slave(1)@10.22.211.18:5051
				Hostname string `json:"hostname"` // ex: 10.22.211.18
			} `json:"slaves"`
		}
		state := &State{}
		err = json.Unmarshal(blob, state)
		if err != nil {
			return err
		}
		for _, slave := range state.Slaves {
			if slave.Pid != "" {
				if parts := strings.SplitN(slave.Pid, "@", 2); len(parts) == 2 && len(parts[1]) > 0 {
					hosts = append(hosts, parts[1])
				} else {
					log.Warningf("unparsable slave pid: %v", slave.Pid)
				}
			}
		}
		return nil
	})
	return hosts, err
}

// return a list of slaves running a k8sm kubelet/executor
func (c *mesosClient) EnlistedSlaves(ctx context.Context) ([]string, error) {
	slaves, err := c.EnumerateSlaves(ctx)
	if err != nil {
		return nil, err
	}

	//TODO(jdef) should parallelize this
	results := []string{}
	for _, slave := range slaves {
		if found, err := c.slaveRunningKubeletExecutor(ctx, slave); found {
			// parse the host from the slave host:port
			if host, _, err := net.SplitHostPort(slave); err == nil {
				results = append(results, host)
			} else {
				log.V(1).Infof("failed to parse slave host from host:port '%v'", slave)
			}
		} else if err != nil {
			// swallow the error and move on to the next
			log.Warningf("failed to test slave for presence of kubelet-executor: %v", err)
		}
	}
	return results, nil
}

func (c *mesosClient) slaveRunningKubeletExecutor(ctx context.Context, slaveHostPort string) (bool, error) {
	uri := fmt.Sprintf("http://%s/state.json", slaveHostPort)
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return false, err
	}
	found := false
	err = c.httpDo(ctx, req, func(res *http.Response, err error) error {
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("HTTP request failed with code %d: %v", res.StatusCode, res.Status)
		}
		blob, err1 := ioutil.ReadAll(res.Body)
		if err1 != nil {
			return err1
		}
		log.V(3).Infof("Got mesos slave state, content length %v", len(blob))
		type State struct {
			Frameworks []*struct {
				Executors []*struct {
					ID     string `json:"id"`
					Source string `json:"source"`
				} `json:"executors"`
			} `json:"frameworks"`
		}
		state := &State{}
		err = json.Unmarshal(blob, state)
		if err != nil {
			return err
		}
		for _, f := range state.Frameworks {
			for _, e := range f.Executors {
				if e.Source == config.DefaultInfoSource && e.ID == config.DefaultInfoID {
					found = true
					return nil
				}
			}
		}
		return nil
	})
	return found, err
}

type responseHandler func(*http.Response, error) error

// hacked from https://blog.golang.org/context
func (c *mesosClient) httpDo(ctx context.Context, req *http.Request, f responseHandler) error {
	// Run the HTTP request in a goroutine and pass the response to f.
	ch := make(chan error, 1)
	go func() { ch <- f(c.client.Do(req)) }()
	select {
	case <-ctx.Done():
		c.tr.CancelRequest(req)
		<-ch // Wait for f to return.
		return ctx.Err()
	case err := <-ch:
		return err
	}
}
