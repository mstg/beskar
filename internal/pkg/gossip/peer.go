// SPDX-FileCopyrightText: Copyright (c) 2023, CIQ, Inc. All rights reserved
// SPDX-License-Identifier: Apache-2.0

package gossip

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
	"go.ciq.dev/beskar/internal/pkg/config"
	"go.ciq.dev/beskar/pkg/mtls"
	"go.ciq.dev/beskar/pkg/netutil"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	GossipLabelKey = "go.ciq.dev/beskar-gossip"
	namespaceFile  = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

func Start(beskarConfig *config.BeskarConfig, client kubernetes.Interface, timeout time.Duration) (*Member, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}

	peers, err := getPeers(beskarConfig, client, timeout)
	if err != nil {
		return nil, err
	}
	key, err := getKey(beskarConfig)
	if err != nil {
		return nil, err
	}
	meta, err := getMeta(beskarConfig)
	if err != nil {
		return nil, err
	}
	state, err := getState(beskarConfig, len(peers))
	if err != nil {
		return nil, err
	}

	host, port, err := net.SplitHostPort(beskarConfig.Gossip.Addr)
	if err != nil {
		return nil, err
	} else if host == "" {
		host = "0.0.0.0"
	}

	return NewMember(
		id.String(),
		peers,
		WithBindAddress(net.JoinHostPort(host, port)),
		WithSecretKey(key),
		WithNodeMeta(meta),
		WithLocalState(state),
	)
}

func getKey(beskarConfig *config.BeskarConfig) ([]byte, error) {
	return base64.StdEncoding.DecodeString(beskarConfig.Gossip.Key)
}

func getMeta(beskarConfig *config.BeskarConfig) ([]byte, error) {
	_, port, err := net.SplitHostPort(beskarConfig.Cache.Addr)
	if err != nil {
		return nil, err
	}

	meta := NewBeskarMeta()
	cachePort, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("while parsing cache address: %w", err)
	}

	meta.CachePort = uint16(cachePort)

	return meta.Encode()
}

func getState(beskarConfig *config.BeskarConfig, numPeers int) ([]byte, error) {
	if numPeers == 0 || !beskarConfig.RunInKubernetes() {
		caCert, caKey, err := mtls.GenerateCA("beskar", time.Now().AddDate(10, 0, 0), mtls.ECDSAKey)
		if err != nil {
			return nil, err
		}
		return mtls.MarshalCAPEM(&mtls.CAPEM{
			Cert: caCert,
			Key:  caKey,
		})
	}
	return nil, nil
}

func getPeers(beskarConfig *config.BeskarConfig, client kubernetes.Interface, timeout time.Duration) ([]string, error) {
	if !beskarConfig.RunInKubernetes() {
		return beskarConfig.Gossip.Peers, nil
	}

	data, err := os.ReadFile(namespaceFile)
	if err != nil {
		return nil, err
	}

	namespace := string(bytes.TrimSpace(data))

	if client == nil {
		inCluster, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("while getting k8s cluster configuration: %w", err)
		}
		client, err = kubernetes.NewForConfig(inCluster)
		if err != nil {
			return nil, fmt.Errorf("while instantiating k8s client: %w", err)
		}
	}

	podIP, err := netutil.RouteGetSourceAddress(os.Getenv("KUBERNETES_SERVICE_HOST"))
	if err != nil {
		return nil, err
	}

	var peers []string

	getPeers := func() error {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(5*time.Second))
		defer cancel()

		endpointList, err := client.CoreV1().Endpoints(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labels.Set(map[string]string{
				GossipLabelKey: "true",
			}).String(),
		})
		if err != nil {
			return fmt.Errorf("while listing endpoints: %w", err)
		}

		var subsetIPs []string
		gossipPort := int32(0)
		peers = nil

		for _, ep := range endpointList.Items {
			for _, subset := range ep.Subsets {
				for _, port := range subset.Ports {
					if port.Protocol != v1.ProtocolTCP {
						continue
					}
					gossipPort = port.Port
					break
				}
				for _, address := range subset.Addresses {
					subsetIPs = append(subsetIPs, address.IP)
				}
			}
		}

		if gossipPort == 0 {
			return fmt.Errorf("no gossip port found")
		}

		for _, ip := range subsetIPs {
			if ip == podIP {
				continue
			}
			peer := net.JoinHostPort(ip, fmt.Sprintf("%d", gossipPort))
			peers = append(peers, peer)
		}

		if len(subsetIPs) == 0 {
			return fmt.Errorf("no gossip peer found")
		}

		return nil
	}

	eb := backoff.NewExponentialBackOff()
	eb.MaxElapsedTime = timeout

	return peers, backoff.RetryNotify(getPeers, eb, func(err error, backoff time.Duration) {})
}
