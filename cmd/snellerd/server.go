// Copyright (C) 2022 Sneller, Inc.
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/SnellerInc/sneller/auth"
	"github.com/SnellerInc/sneller/plan"
	"github.com/SnellerInc/sneller/tenant"
)

type cachedEnv interface {
	plan.Env
	CacheValues() ([]byte, time.Time)
}

type contextKey struct {
	key string
}

var rawConnKey = &contextKey{key: "rawConn"}

type server struct {
	logger  *log.Logger
	manager *tenant.Manager

	sandbox   bool
	cachedir  string
	tenantcmd []string

	peers peerlist
	auth  auth.Provider

	// when we encounter an error
	// listing peers, we fall back to
	// this list (assuming it is non-nil)

	// split size used to configure the splitter,
	// can be left 0 to use the default
	splitSize int64

	// when started, the http server
	srv http.Server
	// when started, the address of the http listener
	// and the tenant remote socket, respectively
	bound, remote net.Addr

	// stale stores either 'nil'
	// or a []*net.TCPAddr returned
	// from peers.EndPoints(); we use this
	// if the end point errors out
	stale atomic.Value

	// hack to avoid data races in testing
	aboutToServe func()
}

func (s *server) Close() error {
	s.manager.Stop()
	s.peers.Stop()
	s.srv.Close()
	return nil
}

func (s *server) Shutdown(ctx context.Context) error {
	if s.manager != nil {
		s.manager.Stop()
		s.manager = nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *server) handler() *http.ServeMux {
	r := http.NewServeMux()
	r.HandleFunc("/", s.handle(s.versionHandler, http.MethodGet))
	r.HandleFunc("/ping", s.handle(s.pingHandler, http.MethodGet))
	r.HandleFunc("/executeQuery", s.handle(s.executeQueryHandler, http.MethodHead, http.MethodGet, http.MethodPost))
	r.HandleFunc("/databases", s.handle(s.databasesHandler, http.MethodGet))
	r.HandleFunc("/tables", s.handle(s.tablesHandler, http.MethodGet))
	r.HandleFunc("/inputs", s.handle(s.inputsHandler, http.MethodGet))
	return r
}

func (s *server) Serve(httpsock, tenantsock net.Listener) error {
	s.manager = tenant.NewManager(s.tenantcmd,
		tenant.WithLogger(s.logger),
		tenant.WithRemote(tenantsock),
	)
	s.manager.Sandbox = s.sandbox
	s.manager.CacheDir = s.cachedir
	if tenantsock != nil {
		go func() {
			if err := s.manager.Serve(); err != nil {
				s.logger.Fatal(err)
			}
		}()
	}
	s.bound = httpsock.Addr()
	if tenantsock != nil {
		s.remote = tenantsock.Addr()
	}
	s.srv.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		return context.WithValue(ctx, rawConnKey, conn)
	}
	s.srv.Handler = s.handler()
	if s.aboutToServe != nil {
		s.aboutToServe()
	}
	return s.srv.Serve(httpsock)
}
