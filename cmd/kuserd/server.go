/*
 * Copyright 2017-2019 Kopano and its licensors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *	http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/longsleep/go-metrics/loggedwriter"
	"github.com/longsleep/go-metrics/timing"
	"github.com/sirupsen/logrus"

	"stash.kopano.io/kgol/kcc-go"
)

// Server represents the base for a HTTP server providing web service endpoints
// utilizing Kopano Server via kcc.
type Server struct {
	c          *kcc.KCC
	listenAddr string
	logger     logrus.FieldLogger

	session            *kcc.Session
	sessionMutex       sync.RWMutex
	withRequestMetrics bool
}

// NewServer creates a new Server with the provided parameters.
func NewServer(listenAddr string, serverURI *url.URL, logger logrus.FieldLogger) *Server {
	s := &Server{
		c:          kcc.NewKCC(serverURI),
		listenAddr: listenAddr,
		logger:     logger,
	}
	s.c.SetClientApp("kcc-go-kuserd", kcc.Version)

	logger.WithField("client", s.c.String()).Infoln("backend server connection set up")

	return s
}

func (s *Server) addContext(parent context.Context, next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		// Create per request context.
		ctx, cancel := context.WithCancel(parent)
		loggedWriter := metrics.NewLoggedResponseWriter(rw)

		if s.withRequestMetrics {
			// Create per request context.
			ctx = timing.NewContext(ctx, func(duration time.Duration) {
				// This is the stop callback, called when complete with duration.
				durationMs := float64(duration) / float64(time.Millisecond)
				// Log request.
				s.logger.WithFields(logrus.Fields{
					"status":     loggedWriter.Status(),
					"method":     req.Method,
					"path":       req.URL.Path,
					"remote":     req.RemoteAddr,
					"duration":   durationMs,
					"user-agent": req.UserAgent(),
				}).Debug("HTTP request complete")
			})
		}
		// Run the request.
		next.ServeHTTP(loggedWriter, req.WithContext(ctx))
		// Cancel per request context when done.
		cancel()
	})
}

func (s *Server) setSession(session *kcc.Session) {
	s.sessionMutex.Lock()
	s.session = session
	s.sessionMutex.Unlock()
}

func (s *Server) getSession() *kcc.Session {
	s.sessionMutex.RLock()
	session := s.session
	s.sessionMutex.RUnlock()
	return session
}

// Serve is the accociated Server's main blocking runner.
func (s *Server) Serve(ctx context.Context, username string, password string) error {
	serveCtx, serveCtxCancel := context.WithCancel(ctx)
	defer serveCtxCancel()

	logger := s.logger

	errCh := make(chan error, 2)
	exitCh := make(chan bool, 1)
	signalCh := make(chan os.Signal)

	http.Handle("/logon", s.addContext(serveCtx, http.HandlerFunc(s.logonHandler)))
	http.Handle("/logoff", s.addContext(serveCtx, http.HandlerFunc(s.logoffHandler)))
	http.Handle("/userinfo", s.addContext(serveCtx, http.HandlerFunc(s.userinfoHandler)))
	http.Handle("/error", s.addContext(serveCtx, http.HandlerFunc(s.errorSenseHandler)))
	http.Handle("/errors", s.addContext(serveCtx, http.HandlerFunc(s.errorsList)))
	http.Handle("/ab-resolve-names", s.addContext(serveCtx, http.HandlerFunc(s.abResolveNamesHandler)))

	// HTTP listener.
	srv := &http.Server{
		Handler: http.DefaultServeMux,
	}

	if username != "" {
		logger.WithField("username", username).Infoln("server session enabled")
		go func() {
			retry := time.NewTimer(5 * time.Second)
			retry.Stop()
			refreshCh := make(chan bool, 1)
			for {
				s.setSession(nil)
				session, sessionErr := kcc.NewSession(serveCtx, s.c, username, password)
				if sessionErr != nil {
					logger.WithError(sessionErr).Errorln("failed to create server session")
					retry.Reset(5 * time.Second)
				} else {
					s.logger.Debugf("server session established: %v", session)
					s.setSession(session)
					go func() {
						<-session.Context().Done()
						s.logger.Debugf("server session has ended: %v", session)
						refreshCh <- true
					}()
				}

				select {
				case <-refreshCh:
					// will retry instantly.
				case <-retry.C:
					// will retry instantly.
				case <-exitCh:
					// give up.
					return
				}
			}
		}()
	}

	logger.WithField("listenAddr", s.listenAddr).Infoln("starting http listener")
	listener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return err
	}

	logger.Infoln("ready to handle requests")

	go func() {
		serveErr := srv.Serve(listener)
		if serveErr != nil {
			errCh <- serveErr
		}

		logger.Debugln("http listener stopped")
		close(exitCh)
	}()

	// Wait for exit or error.
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err = <-errCh:
		// breaks
	case reason := <-signalCh:
		logger.WithField("signal", reason).Warnln("received signal")
		// breaks
	}

	// Shutdown, server will stop to accept new connections, requires Go 1.8+.
	logger.Infoln("clean server shutdown start")
	shutDownCtx, shutDownCtxCancel := context.WithTimeout(ctx, 10*time.Second)
	if shutdownErr := srv.Shutdown(shutDownCtx); shutdownErr != nil {
		logger.WithError(shutdownErr).Warn("clean server shutdown failed")
	}

	// Cancel our own context, wait on managers.
	serveCtxCancel()
	func() {
		for {
			select {
			case <-exitCh:
				return
			default:
				// HTTP listener has not quit yet.
				logger.Info("waiting for http listener to exit")
			}
			select {
			case reason := <-signalCh:
				logger.WithField("signal", reason).Warn("received signal")
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	shutDownCtxCancel() // prevent leak.

	return err
}
