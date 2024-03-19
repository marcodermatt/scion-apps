// Copyright 2021 ETH Zurich
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pan

import (
	"context"
	"github.com/scionproto/scion/pkg/addr"
	"github.com/scionproto/scion/pkg/experimental/fabrid"
	"github.com/scionproto/scion/pkg/log"
	"github.com/scionproto/scion/pkg/private/serrors"
	"github.com/scionproto/scion/pkg/slayers"
	"github.com/scionproto/scion/pkg/slayers/extension"
	"github.com/scionproto/scion/pkg/snet"
	"google.golang.org/grpc"
	"net"
	"net/netip"

	"github.com/scionproto/scion/pkg/snet"
)

// Conn represents a _dialed_ connection.
type Conn interface {
	net.Conn
	// SetPolicy allows to set the path policy for paths used by Write, at any
	// time.
	SetPolicy(policy Policy)
	// WriteVia writes a message to the remote address via the given path.
	// This bypasses the path policy and selector used for Write.
	WriteVia(path *Path, b []byte) (int, error)
	// ReadVia reads a message and returns the (return-)path via which the
	// message was received.
	ReadVia(b []byte) (int, *Path, error)

	GetPath() *Path
}

// DialUDP opens a SCION/UDP socket, connected to the remote address.
// If the local address, or either its IP or port, are left unspecified, they
// will be automatically chosen.
//
// DialUDP looks up SCION paths to the destination AS. The policy defines the
// allowed paths and their preference order. The selector dynamically selects
// a path among this set for each Write operation.
// If the policy is nil, all paths are allowed.
// If the selector is nil, a DefaultSelector is used.
func DialUDP(ctx context.Context, local netip.AddrPort, remote UDPAddr,
	policy Policy, selector Selector) (Conn, error) {

	local, err := defaultLocalAddr(local)
	if err != nil {
		return nil, err
	}

	sn := snet.SCIONNetwork{
		Topology:    host().sciond,
		SCMPHandler: scmpHandler{},
	}
	conn, err := sn.OpenRaw(ctx, net.UDPAddrFromAddrPort(local))
	if err != nil {
		return nil, err
	}
	ipport := conn.LocalAddr().(*net.UDPAddr).AddrPort()
	localUDPAddr := UDPAddr{
		IA:   host().ia,
		IP:   ipport.Addr(),
		Port: ipport.Port(),
	}
	var subscriber *pathRefreshSubscriber
	if remote.IA != localUDPAddr.IA {
		if selector == nil {
			selector = NewDefaultSelector()
		}
		subscriber, err = openPathRefreshSubscriber(ctx, localUDPAddr, remote, policy, selector)
		if err != nil {
			return nil, err
		}
	}
	return &dialedConn{
		baseUDPConn: baseUDPConn{
			raw: conn,
		},
		local:      localUDPAddr,
		remote:     remote,
		subscriber: subscriber,
		selector:   selector,
	}, nil
}

func DialUDPWithFabrid(ctx context.Context, local netip.AddrPort, remote UDPAddr,
	policy Policy, fabridConfig fabrid.SimpleFabridConfig) (Conn, *fabrid.Client, error) {

	servicesInfo, err := host().sciond.SVCInfo(ctx, []addr.SVC{addr.SvcCS})
	if err != nil {
		return nil, nil, serrors.WrapStr("Error getting services", err)
	}
	controlServiceInfo := servicesInfo[addr.SvcCS][0]
	localAddr := &net.TCPAddr{
		IP:   net.IP(local.Addr().AsSlice()),
		Port: 0,
	}
	controlAddr, err := net.ResolveTCPAddr("tcp", controlServiceInfo)
	if err != nil {
		return nil, nil, serrors.WrapStr("Error resolving CS", err)
	}

	log.Debug("Prepared GRPC connection", "CS", controlServiceInfo)
	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		return net.DialTCP("tcp", localAddr, controlAddr)
	}
	grpcconn, err := grpc.DialContext(ctx, controlServiceInfo,
		grpc.WithInsecure(), grpc.WithContextDialer(dialer))
	if err != nil {
		return nil, nil, serrors.WrapStr("Error connecting to CS", err)
	}

	local, err = defaultLocalAddr(local)
	fabridConfig.LocalIA = addr.IA(host().ia)
	fabridConfig.LocalAddr = local.Addr().String()
	client := fabrid.NewFabridClient(*remote.snetUDPAddr(), fabridConfig, grpcconn)

	selector := NewFabridSelector(client, ctx)

	if err != nil {
		return nil, nil, err
	}

	raw, slocal, err := openBaseUDPConn(ctx, local)
	if err != nil {
		return nil, nil, err
	}
	var subscriber *pathRefreshSubscriber
	if remote.IA != slocal.IA {
		subscriber, err = openPathRefreshSubscriber(ctx, slocal, remote, policy, selector)
		if err != nil {
			return nil, nil, err
		}
	} else {
		log.Info("Not using FABRID for local traffic")
	}
	return &fabridDialedConn{
		dialedConn: dialedConn{
			baseUDPConn: baseUDPConn{
				raw: raw,
			},
			local:      slocal,
			remote:     remote,
			subscriber: subscriber,
			selector:   selector,
		},
		fabridClient: client,
	}, client, nil
}

type dialedConn struct {
	baseUDPConn

	local      UDPAddr
	remote     UDPAddr
	subscriber *pathRefreshSubscriber
	selector   Selector
}

type fabridDialedConn struct {
	dialedConn

	fabridClient *fabrid.Client
}

func (c *dialedConn) SetPolicy(policy Policy) {
	if c.subscriber != nil {
		c.subscriber.setPolicy(policy)
	}
}

func (c *dialedConn) LocalAddr() net.Addr {
	return c.local
}

func (c *dialedConn) GetPath() *Path {
	if c.selector == nil {
		return nil
	}
	return c.selector.Path()
}

func (c *dialedConn) RemoteAddr() net.Addr {
	return c.remote
}

func (c *dialedConn) Write(b []byte) (int, error) {
	var path *Path
	if c.local.IA != c.remote.IA {
		path = c.selector.Path()
		if path == nil {
			return 0, errNoPathTo(c.remote.IA)
		}
	}
	return c.baseUDPConn.writeMsg(c.local, c.remote, path, b, nil)
}

func (c *dialedConn) WriteVia(path *Path, b []byte) (int, error) {
	return c.baseUDPConn.writeMsg(c.local, c.remote, path, b, nil)
}

func (c *dialedConn) Read(b []byte) (int, error) {
	for {
		n, remote, _, _, _, err := c.baseUDPConn.readMsg(b)
		if err != nil {
			return n, err
		}
		if remote != c.remote {
			continue // connected! Ignore spurious packets from wrong source
		}
		return n, err
	}
}

func (c *dialedConn) ReadVia(b []byte) (int, *Path, error) {
	for {
		n, remote, fwPath, _, _, err := c.baseUDPConn.readMsg(b)
		if err != nil {
			return n, nil, err
		}
		if remote != c.remote {
			continue // connected! Ignore spurious packets from wrong source
		}
		path, err := reversePathFromForwardingPath(c.remote.IA, c.local.IA, fwPath)
		if err != nil {
			continue // just drop the packet if there is something wrong with the path
		}
		return n, path, nil
	}
}

func (c *dialedConn) Close() error {
	if c.subscriber != nil {
		_ = c.subscriber.Close()
	}
	if c.selector != nil {
		_ = c.selector.Close()
	}
	return c.baseUDPConn.Close()
}

func (c *fabridDialedConn) Read(b []byte) (int, error) {
	for {
		n, remote, fwPath, _, e2eExt, err := c.baseUDPConn.readMsg(b)
		if err != nil {
			return n, err
		}
		if remote != c.remote {
			continue // connected! Ignore spurious packets from wrong source
		}

		// Check extensions for relevant options
		var fabridControlOption *extension.FabridControlOption
		if e2eExt != nil {
			path, err := reversePathFromForwardingPath(c.remote.IA, c.local.IA, fwPath)
			if err != nil {
				return 0, err
			}
			for _, opt := range e2eExt.Options {
				switch opt.OptType {
				case slayers.OptTypeFabridControl:
					fabridControlOption, err = extension.ParseFabridControlOption(opt, nil)
					if err != nil {
						return n, err
					}

					err = c.fabridClient.HandleFabridControlOption(snet.PathFingerprint(path.Fingerprint), fabridControlOption)
					if err != nil {
						return 0, err
					}

				}
			}
		}
		if fabridControlOption != nil {
			if n == 0 {
				continue // Don't return empty packets that contain FABRID options
			}
		}
		return n, err
	}
}

// pathRefreshSubscriber is the glue between a connection and the global path
// pool. It gets the paths to dst and sets the filtered path set on the
// target Selector.
type pathRefreshSubscriber struct {
	remoteIA IA
	policy   Policy
	target   Selector
}

func openPathRefreshSubscriber(ctx context.Context, local, remote UDPAddr, policy Policy,
	target Selector) (*pathRefreshSubscriber, error) {

	s := &pathRefreshSubscriber{
		remoteIA: remote.IA,
		policy:   policy,
		target:   target,
	}
	paths, err := pool.subscribe(ctx, remote.IA, s)
	if err != nil {
		return nil, err
	}
	s.target.Initialize(local, remote, filtered(s.policy, paths))
	return s, nil
}

func (s *pathRefreshSubscriber) Close() error {
	pool.unsubscribe(s.remoteIA, s)
	return nil
}

func (s *pathRefreshSubscriber) setPolicy(policy Policy) {
	s.policy = policy
	paths := pool.cachedPaths(s.remoteIA)
	s.target.Refresh(filtered(s.policy, paths))
}

func (s *pathRefreshSubscriber) refresh(dst IA, paths []*Path) {
	s.target.Refresh(filtered(s.policy, paths))
}

func (s *pathRefreshSubscriber) PathDown(pf PathFingerprint, pi PathInterface) {
	s.target.PathDown(pf, pi)
}

func filtered(policy Policy, paths []*Path) []*Path {
	if policy != nil {
		return policy.Filter(paths)
	}
	return paths
}
