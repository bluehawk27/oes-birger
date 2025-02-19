/*
 * Copyright 2021 OpsMx, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License")
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package tunnelroute

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"go.uber.org/zap"
)

var (
	rnd = rand.New(rand.NewSource(time.Now().UnixNano())) // not used for crypto
)

// BaseStatistics defines the standard statistics returned for every
// route type.  This should be included in the specific route types,
// such as "directly connected" or "on other controller" route connections.
type BaseStatistics struct {
	Name           string     `json:"name,omitempty"`
	Session        string     `json:"session,omitempty"`
	ConnectionType string     `json:"connectionType,omitempty"`
	Endpoints      []Endpoint `json:"endpoints,omitempty"`
	Version        string     `json:"version,omitempty"`
	Hostname       string     `json:"hostname,omitempty"`
}

// Route is a thing that looks like a connected route (agent), either directly connected or
// through another controller.
type Route interface {
	Close()
	Send(interface{}) string
	Cancel(string)
	HasEndpoint(string, string) bool
	GetSession() string
	GetName() string
	GetEndpoints() []Endpoint

	GetStatistics() interface{}
}

// ConnectedRoutes holds a list of all currently connected or known routes (agents)
type ConnectedRoutes struct {
	sync.RWMutex
	m map[string][]Route
}

// GetStatistics returns statistics for all routes currently connected.
// The statistics returned is an opaque object, intended to be rendered to JSON.
func (s *ConnectedRoutes) GetStatistics() interface{} {
	ret := make([]interface{}, 0)
	s.RLock()
	defer s.RUnlock()
	for _, routeList := range s.m {
		for _, route := range routeList {
			ret = append(ret, route.GetStatistics())
		}
	}
	return ret
}

// MakeRoutes returns a new Routes object which will manage (safely) routes, such as agents,
// connected directly or indirectly.
func MakeRoutes() *ConnectedRoutes {
	return &ConnectedRoutes{
		m: make(map[string][]Route),
	}
}

func sliceIndex(limit int, predicate func(i int) bool) int {
	for i := 0; i < limit; i++ {
		if predicate(i) {
			return i
		}
	}
	return -1
}

// Add will add a new route to our list.
func (s *ConnectedRoutes) Add(state Route) {
	s.Lock()
	defer s.Unlock()
	routeList, ok := s.m[state.GetName()]
	if !ok {
		routeList = make([]Route, 0)
	}
	routeList = append(routeList, state)
	s.m[state.GetName()] = routeList
	zap.S().Infow("new route",
		"destination", state.GetName(),
		"sessionId", state.GetSession(),
		"pathCount", len(routeList),
		"endpointCount", len(state.GetEndpoints()))
	for _, endpoint := range state.GetEndpoints() {
		zap.S().Infow("endpoint",
			"destination", state.GetName(),
			"sessionId", state.GetSession(),
			"endpointType", endpoint.Type,
			"endpointName", endpoint.Name,
			"endpointConfigured", endpoint.Configured)
	}
	connectedRoutesGauge.WithLabelValues(state.GetName()).Inc()
}

// Remove will remove a route and signal to it that closing down is started.
//
// Rather than return an error here, we will just log it.  This is because we
// won't likely care in the caller, so there's no need to burden them with
// an if statement just to check it.
func (s *ConnectedRoutes) Remove(state Route) {
	s.Lock()
	defer s.Unlock()

	state.Close()

	routeList, ok := s.m[state.GetName()]
	if !ok {
		// This should not be possible.
		zap.S().Errorf("no routes known by the name of %s", state)
		return
	}

	// TODO: We should always find our entry...
	i := sliceIndex(len(routeList), func(i int) bool { return routeList[i] == state })
	if i == -1 {
		zap.S().Errorf("attempt to remove unknown route %s", state)
		return
	}
	routeList[i] = routeList[len(routeList)-1]
	routeList[len(routeList)-1] = nil
	routeList = routeList[:len(routeList)-1]
	s.m[state.GetName()] = routeList
	connectedRoutesGauge.WithLabelValues(state.GetName()).Dec()
	zap.S().Infow("remove route",
		"destination", state.GetName(),
		"sessionId", state.GetSession(),
		"pathCount", len(routeList))
}

func (s *ConnectedRoutes) findService(ep Search) (Route, error) {
	routeList, ok := s.m[ep.Name]
	if !ok || len(routeList) == 0 {
		return nil, fmt.Errorf("no routes connected for %s", ep)
	}
	possibleRoutes := []int{}
	for i, a := range routeList {
		if a.HasEndpoint(ep.EndpointType, ep.EndpointName) {
			possibleRoutes = append(possibleRoutes, i)
		}
	}
	if len(possibleRoutes) == 0 {
		return nil, fmt.Errorf("request for %s, no such route exists or all are unconfigured", ep)
	}
	selected := possibleRoutes[rnd.Intn(len(possibleRoutes))]
	return routeList[selected], nil
}

// Send will search for the specific route and endpoint. send a message to an route, and return true if a route
// was found.
func (s *ConnectedRoutes) Send(ep Search, message interface{}) (string, error) {
	s.RLock()
	defer s.RUnlock()
	route, err := s.findService(ep)
	if err != nil {
		return "", err
	}
	session := route.Send(message)
	return session, nil
}

// Cancel will cancel an ongoing request.
func (s *ConnectedRoutes) Cancel(ep Search, id string) error {
	// The session must be set, if not this is an error.
	if len(ep.Session) == 0 {
		return fmt.Errorf("session is not set (coding error)")
	}

	s.RLock()
	defer s.RUnlock()
	routeList, ok := s.m[ep.Name]
	if !ok || len(routeList) == 0 {
		return fmt.Errorf("no routes connected for: %s (likely coding error)", ep)
	}

	for _, a := range routeList {
		if ep.MatchesRoute(a) {
			a.Cancel(id)
			return nil
		}
	}

	return fmt.Errorf("no routes with specific session exist for %s (likely coding error)", ep)
}
