//     Copyright (C) 2020, IrineSistiana
//
//     This file is part of mosdns.
//
//     mosdns is free software: you can redistribute it and/or modify
//     it under the terms of the GNU General Public License as published by
//     the Free Software Foundation, either version 3 of the License, or
//     (at your option) any later version.
//
//     mosdns is distributed in the hope that it will be useful,
//     but WITHOUT ANY WARRANTY; without even the implied warranty of
//     MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//     GNU General Public License for more details.
//
//     You should have received a copy of the GNU General Public License
//     along with this program.  If not, see <https://www.gnu.org/licenses/>.

package dispatcher

import (
	"context"
	"crypto/x509"
	"fmt"
	"github.com/IrineSistiana/mosdns/dispatcher/config"
	"github.com/IrineSistiana/mosdns/dispatcher/handler"
	_ "github.com/IrineSistiana/mosdns/dispatcher/plugin"
	"github.com/IrineSistiana/mosdns/dispatcher/server"
	"github.com/sirupsen/logrus"
	"io"
	"io/ioutil"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/IrineSistiana/mosdns/dispatcher/logger"

	"github.com/miekg/dns"
)

const (
	queryTimeout = time.Second * 5
)

// Dispatcher represents a dns query dispatcher
type Dispatcher struct {
	config *config.Config
}

// Init inits a dispatcher from configuration
func Init(c *config.Config) (*Dispatcher, error) {
	// init logger
	if len(c.Log.Level) != 0 {
		level, err := logrus.ParseLevel(c.Log.Level)
		if err != nil {
			return nil, err
		}
		logger.GetLogger().SetLevel(level)
	}
	if len(c.Log.File) != 0 {
		f, err := os.OpenFile(c.Log.File, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0755)
		if err != nil {
			return nil, fmt.Errorf("can not open log file %s: %w", c.Log.File, err)
		}
		logger.Entry().Infof("use log file %s", c.Log.File)
		logWriter := io.MultiWriter(os.Stdout, f)
		logger.GetLogger().SetOutput(logWriter)
	}
	if logger.GetLogger().IsLevelEnabled(logrus.DebugLevel) {
		logger.GetLogger().SetReportCaller(true)
		go func() {
			m := new(runtime.MemStats)
			for {
				time.Sleep(time.Second * 15)
				runtime.ReadMemStats(m)
				logger.Entry().Debugf("HeapObjects: %d NumGC: %d PauseTotalNs: %d, NumGoroutine: %d", m.HeapObjects, m.NumGC, m.PauseTotalNs, runtime.NumGoroutine())
			}
		}()
	}

	d := new(Dispatcher)
	d.config = c

	plugins := c.Plugin.Plugin
	plugins = append(plugins, c.Plugin.Router...)
	plugins = append(plugins, c.Plugin.Functional...)
	plugins = append(plugins, c.Plugin.Matcher...)
	for i, pluginConfig := range plugins {
		if len(pluginConfig.Tag) == 0 {
			logger.Entry().Warnf("plugin at index %d has a empty tag, ignore it", i)
			continue
		}
		if err := handler.InitAndRegPlugin(pluginConfig); err != nil {
			return nil, fmt.Errorf("failed to register plugin %d-%s: %w", i, pluginConfig.Tag, err)
		}
		logger.Entry().Debugf("plugin %s loaded", pluginConfig.Tag)
	}

	handler.RegEntry(d.config.Plugin.Entry...)

	return d, nil
}

func (d *Dispatcher) ServeDNS(ctx context.Context, qCtx *handler.Context, w server.ResponseWriter) {
	queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	err := handler.Dispatch(queryCtx, qCtx)

	var r *dns.Msg
	if err != nil {
		qCtx.Logf(logrus.WarnLevel, "query failed: %v", err)
		r = new(dns.Msg)
		r.SetReply(qCtx.Q)
		r.Rcode = dns.RcodeServerFailure
	} else {
		r = qCtx.R
	}

	if r != nil {
		if _, err := w.Write(r); err != nil {
			logger.Entry().Warnf("failed to respond client: %v", err)
		}
	}
}

// StartServer starts mosdns. Will always return a non-nil err.
func (d *Dispatcher) StartServer() error {

	if len(d.config.Server.Bind) == 0 {
		return fmt.Errorf("no address to bind")
	}

	errChan := make(chan error, 1) // must be a buffered chan to catch at least one err.

	for _, s := range d.config.Server.Bind {
		ss := strings.Split(s, "://")
		if len(ss) != 2 {
			return fmt.Errorf("invalid bind address: %s", s)
		}
		network := ss[0]
		addr := ss[1]

		var s server.Server
		switch network {
		case "tcp", "tcp4", "tcp6":
			l, err := net.Listen(network, addr)
			if err != nil {
				return err
			}
			defer l.Close()
			logger.Entry().Infof("tcp server started at %s", l.Addr())

			serverConf := server.Config{
				Listener: l,
			}
			s = server.NewTCPServer(&serverConf)

		case "udp", "udp4", "udp6":
			l, err := net.ListenPacket(network, addr)
			if err != nil {
				return err
			}
			defer l.Close()
			logger.Entry().Infof("udp server started at %s", l.LocalAddr())
			serverConf := server.Config{
				PacketConn:        l,
				MaxUDPPayloadSize: d.config.Server.MaxUDPSize,
			}
			s = server.NewUDPServer(&serverConf)
		default:
			return fmt.Errorf("invalid bind protocol: %s", network)
		}

		go func() {
			err := s.ListenAndServe(d)
			select {
			case errChan <- err:
			default:
			}
		}()
	}

	listenerErr := <-errChan

	return fmt.Errorf("server listener failed and exited: %w", listenerErr)
}

func caPath2Pool(cas []string) (*x509.CertPool, error) {
	rootCAs := x509.NewCertPool()

	for _, ca := range cas {
		pem, err := ioutil.ReadFile(ca)
		if err != nil {
			return nil, fmt.Errorf("ReadFile: %w", err)
		}

		if ok := rootCAs.AppendCertsFromPEM(pem); !ok {
			return nil, fmt.Errorf("AppendCertsFromPEM: no certificate was successfully parsed in %s", ca)
		}
	}
	return rootCAs, nil
}
