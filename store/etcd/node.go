package etcdstore

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	engineapi "github.com/docker/engine-api/client"
	"github.com/docker/go-connections/tlsconfig"
	"gitlab.ricebook.net/platform/core/types"
	"gitlab.ricebook.net/platform/core/utils"
	"golang.org/x/net/context"
)

// get a node from etcd
// and construct it's docker client
// a node must belong to a pod
// and since node is not the smallest unit to user, to get a node we must specify the corresponding pod
// storage path in etcd is `/eru-core/pod/:podname/node/:nodename/info`
func (k *krypton) GetNode(podname, nodename string) (*types.Node, error) {
	key := fmt.Sprintf(nodeInfoKey, podname, nodename)
	resp, err := k.etcd.Get(context.Background(), key, nil)
	if err != nil {
		return nil, err
	}
	if resp.Node.Dir {
		return nil, fmt.Errorf("Node storage path %q in etcd is a directory", key)
	}

	node := &types.Node{}
	if err := json.Unmarshal([]byte(resp.Node.Value), node); err != nil {
		return nil, err
	}

	engine, err := k.makeDockerClient(podname, nodename, node.Endpoint, false)
	if err != nil {
		return nil, err
	}

	node.Engine = engine
	return node, nil
}

// add a node
// save it to etcd
// storage path in etcd is `/eru-core/pod/:podname/node/:nodename/info`
func (k *krypton) AddNode(name, endpoint, podname, cafile, certfile, keyfile string, public bool) (*types.Node, error) {
	if !strings.HasPrefix(endpoint, "tcp://") {
		return nil, fmt.Errorf("Endpoint must starts with tcp:// %q", endpoint)
	}

	_, err := k.GetPod(podname)
	if err != nil {
		return nil, err
	}

	// 如果有tls的证书需要保存就保存一下
	if cafile != "" && certfile != "" && keyfile != "" {
		_, err = k.etcd.Set(context.Background(), fmt.Sprintf(nodeCaKey, podname, name), cafile, nil)
		if err != nil {
			return nil, err
		}
		_, err = k.etcd.Set(context.Background(), fmt.Sprintf(nodeCertKey, podname, name), certfile, nil)
		if err != nil {
			return nil, err
		}
		_, err = k.etcd.Set(context.Background(), fmt.Sprintf(nodeKeyKey, podname, name), keyfile, nil)
		if err != nil {
			return nil, err
		}
	}

	// 尝试加载docker的客户端
	engine, err := k.makeDockerClient(podname, name, endpoint, false)
	if err != nil {
		return nil, err
	}

	info, err := engine.Info(context.Background())
	if err != nil {
		return nil, err
	}

	cpumap := types.CPUMap{}
	for i := 0; i < info.NCPU; i++ {
		cpumap[strconv.Itoa(i)] = 10
	}

	key := fmt.Sprintf(nodeInfoKey, podname, name)
	node := &types.Node{
		Name:     name,
		Endpoint: endpoint,
		Podname:  podname,
		Public:   public,
		CPU:      cpumap,
		Engine:   engine,
	}

	bytes, err := json.Marshal(node)
	if err != nil {
		return nil, err
	}

	_, err = k.etcd.Set(context.Background(), key, string(bytes), nil)
	if err != nil {
		return nil, err
	}

	return node, nil
}

// get all nodes from etcd
// any error will break and return immediately
func (k *krypton) GetAllNodes() ([]*types.Node, error) {
	var (
		nodes []*types.Node
		err   error
	)

	pods, err := k.GetAllPods()
	if err != nil {
		return nodes, err
	}

	for _, pod := range pods {
		ns, err := k.GetNodesByPod(pod.Name)
		if err != nil {
			return nodes, err
		}
		nodes = append(nodes, ns...)
	}
	return nodes, err
}

// get all nodes bound to pod
// here we use podname instead of pod instance
// storage path in etcd is `/eru-core/pod/:podname/node`
func (k *krypton) GetNodesByPod(podname string) ([]*types.Node, error) {
	var (
		nodes []*types.Node
		err   error
	)

	key := fmt.Sprintf(podNodesKey, podname)
	resp, err := k.etcd.Get(context.Background(), key, nil)
	if err != nil {
		return nodes, err
	}
	if !resp.Node.Dir {
		return nil, fmt.Errorf("Node storage path %q in etcd is not a directory", key)
	}

	for _, node := range resp.Node.Nodes {
		nodename := utils.Tail(node.Key)
		n, err := k.GetNode(podname, nodename)
		if err != nil {
			return nodes, err
		}
		nodes = append(nodes, n)
	}
	return nodes, err
}

// update a node, save it to etcd
// storage path in etcd is `/eru-core/pod/:podname/node/:nodename/info`
func (k *krypton) UpdateNode(node *types.Node) error {
	key := fmt.Sprintf(nodeInfoKey, node.Podname, node.Name)
	bytes, err := json.Marshal(node)
	if err != nil {
		return err
	}

	_, err = k.etcd.Set(context.Background(), key, string(bytes), nil)
	if err != nil {
		return err
	}

	return nil
}

// update cpu on a node, either add or substract
// need to lock
func (k *krypton) UpdateNodeCPU(podname, nodename string, cpu types.CPUMap, action string) error {
	lock, err := k.CreateLock(fmt.Sprintf("%s_%s", podname, nodename), 30)
	if err != nil {
		return err
	}

	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()

	nodeKey := fmt.Sprintf(nodeInfoKey, podname, nodename)
	resp, err := k.etcd.Get(context.Background(), nodeKey, nil)
	if err != nil {
		return err
	}
	if resp.Node.Dir {
		return fmt.Errorf("Node storage path %q in etcd is a directory", nodeKey)
	}

	node := &types.Node{}
	if err := json.Unmarshal([]byte(resp.Node.Value), node); err != nil {
		return err
	}

	if action == "add" || action == "+" {
		node.CPU.Add(cpu)
	} else if action == "sub" || action == "-" {
		node.CPU.Sub(cpu)
	}

	bytes, err := json.Marshal(node)
	if err != nil {
		return err
	}

	_, err = k.etcd.Set(context.Background(), nodeKey, string(bytes), nil)
	if err != nil {
		return err
	}

	return nil
}

// cache connections
// otherwise they'll leak
type cache struct {
	sync.Mutex
	clients map[string]*engineapi.Client
}

func (c cache) set(host string, client *engineapi.Client) {
	c.Lock()
	defer c.Unlock()

	c.clients[host] = client
}

func (c cache) get(host string) *engineapi.Client {
	c.Lock()
	defer c.Unlock()
	return c.clients[host]
}

var _cache = cache{clients: make(map[string]*engineapi.Client)}

// use endpoint, cert files path, and api version to create docker client
// we don't check whether this is connectable
func makeRawClient(endpoint, certpath, apiversion string) (*engineapi.Client, error) {
	var cli *http.Client
	if certpath != "" {
		options := tlsconfig.Options{
			CAFile:             filepath.Join(certpath, "ca.pem"),
			CertFile:           filepath.Join(certpath, "cert.pem"),
			KeyFile:            filepath.Join(certpath, "key.pem"),
			InsecureSkipVerify: false,
		}
		tlsc, err := tlsconfig.Client(options)
		if err != nil {
			return nil, err
		}

		cli = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsc,
			},
		}
	}

	log.Debugf("Create new http.Client for %q, %q, %q", endpoint, certpath, apiversion)
	return engineapi.NewClient(endpoint, apiversion, cli, nil)
}

func (k *krypton) makeDockerClient(podname, nodename, endpoint string, force bool) (*engineapi.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		return nil, err
	}

	// try get client, if nil, create a new one
	client := _cache.get(host)
	if client == nil || force {
		dockerCertPath := ""
		// 如果设置了cert path说明需要用tls来连接
		// 那么先检查有没有这些证书, 没有的话要从etcd里dump到本地
		if k.config.Docker.CertPath != "" {
			dockerCertPath = filepath.Join(k.config.Docker.CertPath, host)
			_, err = os.Stat(dockerCertPath)
			// 没有证书, 从etcd里dump
			if os.IsNotExist(err) {
				if err := k.dumpFromEtcd(podname, nodename, dockerCertPath); err != nil {
					return nil, err
				}
			}
		}

		client, err = makeRawClient(endpoint, dockerCertPath, k.config.Docker.APIVersion)
		if err != nil {
			return nil, err
		}

		_cache.set(host, client)
	}

	// timeout in 5 seconds
	// timeout means node is not available
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = client.Info(ctx)
	if err != nil {
		return nil, err
	}

	return client, nil
}

// dump certificated files from etcd to local file system
func (k *krypton) dumpFromEtcd(podname, nodename, certprefix string) error {
	// create necessary directory
	if err := os.MkdirAll(certprefix, 0755); err != nil {
		return err
	}

	// create files
	filenames := []string{"ca.pem", "cert.pem", "key.pem"}
	keyFormats := []string{nodeCaKey, nodeCertKey, nodeKeyKey}
	for i := 0; i < 3; i++ {
		resp, err := k.etcd.Get(context.Background(), fmt.Sprintf(keyFormats[i], podname, nodename), nil)
		if err != nil {
			return err
		}
		if err := utils.SaveFile(resp.Node.Value, filepath.Join(certprefix, filenames[i]), 0444); err != nil {
			return err
		}
	}
	log.Debugf("Dump ca.pem, cert.pem, key.pem from etcd for %q %q to %q", podname, nodename, certprefix)
	return nil
}
