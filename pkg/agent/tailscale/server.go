// Copyright 2024 Daytona Platforms Inc.
// SPDX-License-Identifier: Apache-2.0

package tailscale

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"time"

	cfg "github.com/daytonaio/daytona/cmd/daytona/config"
	apiclient_util "github.com/daytonaio/daytona/internal/util/apiclient"
	"github.com/daytonaio/daytona/pkg/agent/config"
	"github.com/daytonaio/daytona/pkg/apiclient"
	"github.com/daytonaio/daytona/pkg/common"
	"tailscale.com/tsnet"

	log "github.com/sirupsen/logrus"
)

type Server struct {
	Hostname         string
	Server           config.DaytonaServerConfig
	TelemetryEnabled bool
	ClientId         string
}

func (s *Server) Start() error {
	errChan := make(chan error)

	tsnetServer, err := s.connect()
	if err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}

	go func(tsnetServer *tsnet.Server) {
		reconnect := func() {
			// Close the tsnet server and reconnect
			err = tsnetServer.Close()
			if err != nil {
				log.Errorf("Failed to close tsnet server: %v", err)
			}

			tsnetServer, err = s.connect()
			if err != nil {
				log.Errorf("Failed to reconnect: %v", err)
			} else {
				log.Info("Reconnected to server")
			}
		}

		for {
			time.Sleep(5 * time.Second)

			localClient, err := tsnetServer.LocalClient()
			if err != nil {
				log.Errorf("Failed to get local client: %v, %v", err, common.ErrConnection)
				reconnect()
				continue
			}

			status, err := localClient.Status(context.Background())
			if err != nil {
				log.Errorf("Failed to get local client status: %v, %v", err, common.ErrConnection)
				reconnect()
				continue
			}

			if status.CurrentTailnet == nil {
				log.Errorf("Tailscale not connected. %v. Reconnecting...", common.ErrConnection)
				reconnect()
			} else {
				log.Tracef("Connected to server. Status: %v", status)
			}
		}
	}(tsnetServer)

	return <-errChan
}

func (s *Server) getNetworkKey() (string, error) {
	apiClient, err := apiclient_util.GetAgentApiClient(s.Server.ApiUrl, s.Server.ApiKey, s.ClientId, s.TelemetryEnabled)
	if err != nil {
		return "", err
	}

	networkKey, _, err := apiClient.ServerAPI.GenerateNetworkKeyExecute(apiclient.ApiGenerateNetworkKeyRequest{})
	// Retry indefinitely. Used to reconnect to the Daytona Server
	if err != nil {
		log.Tracef("Failed to get network key: %v", err)
		time.Sleep(5 * time.Second)
		return s.getNetworkKey()
	}

	return networkKey.Key, nil
}

func (s *Server) getTsnetServer() (*tsnet.Server, error) {
	configDir, err := cfg.GetConfigDir()
	if err != nil {
		return nil, err
	}

	tsnetServer := &tsnet.Server{
		Hostname:   s.Hostname,
		ControlURL: s.Server.Url,
		Ephemeral:  true,
		Dir:        filepath.Join(configDir, "tsnet"),
	}

	networkKey, err := s.getNetworkKey()
	if err != nil {
		return nil, fmt.Errorf("failed to get network key: %w", err)
	}

	tsnetServer.AuthKey = networkKey

	tsnetServer.RegisterFallbackTCPHandler(func(src, dest netip.AddrPort) (handler func(net.Conn), intercept bool) {
		destPort := dest.Port()

		return func(src net.Conn) {
			defer src.Close()
			dst, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", destPort))
			if err != nil {
				log.Errorf("Dial failed: %v", err)
				return
			}
			defer dst.Close()

			done := make(chan struct{})

			go func() {
				defer src.Close()
				defer dst.Close()
				io.Copy(dst, src)
				done <- struct{}{}
			}()

			go func() {
				defer src.Close()
				defer dst.Close()
				io.Copy(src, dst)
				done <- struct{}{}
			}()

			<-done
			<-done
		}, true
	})

	return tsnetServer, nil
}

func (s *Server) connect() (*tsnet.Server, error) {
	tsnetServer, err := s.getTsnetServer()
	if err != nil {
		return nil, err
	}

	ln, err := tsnetServer.Listen("tcp", ":80")
	if err != nil {
		return nil, err
	}

	go func() {
		err := http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "Ok\n")
		}))
		if err != nil {
			// Trace log because this is expected to fail when disconnected from the Daytona Server
			log.Tracef("Failed to serve: %v", err)
		}
	}()

	return tsnetServer, nil
}
