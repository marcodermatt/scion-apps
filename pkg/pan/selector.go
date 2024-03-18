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
	"fmt"
	drhelper "github.com/scionproto/scion/pkg/daemon/helper"
	"github.com/scionproto/scion/pkg/drkey"
	"github.com/scionproto/scion/pkg/experimental/fabrid"
	"github.com/scionproto/scion/pkg/log"
	"github.com/scionproto/scion/pkg/private/common"
	drpb "github.com/scionproto/scion/pkg/proto/control_plane"
	"github.com/scionproto/scion/pkg/snet"
	snetpath "github.com/scionproto/scion/pkg/snet/path"
	"google.golang.org/grpc"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scionproto/scion/pkg/addr"

	"github.com/netsec-ethz/scion-apps/pkg/pan/internal/ping"
)

// Selector controls the path used by a single **dialed** socket. Stateful.
type Selector interface {
	// Path selects the path for the next packet.
	// Invoked for each packet sent with Write.
	Path() *Path
	// Initialize the selector for a connection with the initial list of paths,
	// filtered/ordered by the Policy.
	// Invoked once during the creation of a Conn.
	Initialize(local, remote UDPAddr, paths []*Path)
	// Refresh updates the paths. This is called whenever the Policy is changed or
	// when paths were about to expire and are refreshed from the SCION daemon.
	// The set and order of paths may differ from previous invocations.
	Refresh([]*Path)
	// PathDown is called whenever an SCMP down notification is received on any
	// connection so that the selector can adapt its path choice. The down
	// notification may be for unrelated paths not used by this selector.
	PathDown(PathFingerprint, PathInterface)
	Close() error
}

// DefaultSelector is a Selector for a single dialed socket.
// This will keep using the current path, starting with the first path chosen
// by the policy, as long possible.
// Faults are detected passively via SCMP down notifications; whenever such
// a down notification affects the current path, the DefaultSelector will
// switch to the first path (in the order defined by the policy) that is not
// affected by down notifications.
type DefaultSelector struct {
	mutex   sync.Mutex
	paths   []*Path
	current int
}

func NewDefaultSelector() *DefaultSelector {
	return &DefaultSelector{}
}

func (s *DefaultSelector) Path() *Path {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if len(s.paths) == 0 {
		return nil
	}
	return s.paths[s.current]
}

func (s *DefaultSelector) Initialize(local, remote UDPAddr, paths []*Path) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.paths = paths
	s.current = 0
}

func (s *DefaultSelector) Refresh(paths []*Path) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	newcurrent := 0
	if len(s.paths) > 0 {
		currentFingerprint := s.paths[s.current].Fingerprint
		for i, p := range paths {
			if p.Fingerprint == currentFingerprint {
				newcurrent = i
				break
			}
		}
	}
	s.paths = paths
	s.current = newcurrent
}

func (s *DefaultSelector) PathDown(pf PathFingerprint, pi PathInterface) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if current := s.paths[s.current]; isInterfaceOnPath(current, pi) || pf == current.Fingerprint {
		fmt.Println("down:", s.current, len(s.paths))
		better := stats.FirstMoreAlive(current, s.paths)
		if better >= 0 {
			// Try next path. Note that this will keep cycling if we get down notifications
			s.current = better
			fmt.Println("failover:", s.current, len(s.paths))
		}
	}
}

func (s *DefaultSelector) Close() error {
	return nil
}

type PingingSelector struct {
	// Interval for pinging. Must be positive.
	Interval time.Duration
	// Timeout for the individual pings. Must be positive and less than Interval.
	Timeout time.Duration

	mutex   sync.Mutex
	paths   []*Path
	current int
	local   scionAddr
	remote  scionAddr

	numActive    int64
	pingerCtx    context.Context
	pingerCancel context.CancelFunc
	pinger       *ping.Pinger
}

// SetActive enables active pinging on at most numActive paths.
func (s *PingingSelector) SetActive(numActive int) {
	s.ensureRunning()
	atomic.SwapInt64(&s.numActive, int64(numActive))
}

func (s *PingingSelector) Path() *Path {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if len(s.paths) == 0 {
		return nil
	}
	return s.paths[s.current]
}

func (s *PingingSelector) Initialize(local, remote UDPAddr, paths []*Path) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.local = local.scionAddr()
	s.remote = remote.scionAddr()
	s.paths = paths
	s.current = stats.LowestLatency(s.remote, s.paths)
}

func (s *PingingSelector) Refresh(paths []*Path) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.paths = paths
	s.current = stats.LowestLatency(s.remote, s.paths)
}

func (s *PingingSelector) PathDown(pf PathFingerprint, pi PathInterface) {
	s.reselectPath()
}

func (s *PingingSelector) reselectPath() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.current = stats.LowestLatency(s.remote, s.paths)
}

func (s *PingingSelector) ensureRunning() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.local.IA == s.remote.IA {
		return
	}
	if s.pinger != nil {
		return
	}
	s.pingerCtx, s.pingerCancel = context.WithCancel(context.Background())
	local := s.local.snetUDPAddr()
	pinger, err := ping.NewPinger(s.pingerCtx, host().sciond, local)
	if err != nil {
		return
	}
	s.pinger = pinger
	go s.pinger.Drain(s.pingerCtx)
	go s.run()
}

func (s *PingingSelector) run() {
	pingTicker := time.NewTicker(s.Interval)
	pingTimeout := time.NewTimer(0)
	if !pingTimeout.Stop() {
		<-pingTimeout.C // drain initial timer event
	}

	var sequenceNo uint16
	replyPending := make(map[PathFingerprint]struct{})

	for {
		select {
		case <-s.pingerCtx.Done():
			return
		case <-pingTicker.C:
			numActive := int(atomic.LoadInt64(&s.numActive))
			if numActive > len(s.paths) {
				numActive = len(s.paths)
			}
			if numActive == 0 {
				continue
			}

			activePaths := s.paths[:numActive]
			for _, p := range activePaths {
				replyPending[p.Fingerprint] = struct{}{}
			}
			sequenceNo++
			s.sendPings(activePaths, sequenceNo)
			resetTimer(pingTimeout, s.Timeout)
		case r := <-s.pinger.Replies:
			s.handlePingReply(r, replyPending, sequenceNo)
			if len(replyPending) == 0 {
				pingTimeout.Stop()
				s.reselectPath()
			}
		case <-pingTimeout.C:
			if len(replyPending) == 0 {
				continue // already handled above
			}
			for pf := range replyPending {
				stats.RecordLatency(s.remote, pf, s.Timeout)
				delete(replyPending, pf)
			}
			s.reselectPath()
		}
	}
}

func (s *PingingSelector) sendPings(paths []*Path, sequenceNo uint16) {
	for _, p := range paths {
		remote := s.remote.snetUDPAddr()
		remote.Path = p.ForwardingPath.dataplanePath
		remote.NextHop = net.UDPAddrFromAddrPort(p.ForwardingPath.underlay)
		err := s.pinger.Send(s.pingerCtx, remote, sequenceNo, 16)
		if err != nil {
			panic(err)
		}
	}
}

func (s *PingingSelector) handlePingReply(reply ping.Reply,
	expectedReplies map[PathFingerprint]struct{},
	expectedSequenceNo uint16) {
	if reply.Error != nil {
		// handle NotifyPathDown.
		// The Pinger is not using the normal scmp handler in raw.go, so we have to
		// reimplement this here.
		pf, err := reversePathFingerprint(reply.Path)
		if err != nil {
			return
		}
		switch e := reply.Error.(type) { //nolint:errorlint
		case ping.InternalConnectivityDownError:
			pi := PathInterface{
				IA:   IA(e.IA),
				IfID: IfID(e.Egress),
			}
			stats.NotifyPathDown(pf, pi)
		case ping.ExternalInterfaceDownError:
			pi := PathInterface{
				IA:   IA(e.IA),
				IfID: IfID(e.Interface),
			}
			stats.NotifyPathDown(pf, pi)
		}
		return
	}

	if reply.Source.Host.Type() != addr.HostTypeIP {
		return // ignore replies from non-IP addresses
	}
	src := scionAddr{
		IA: IA(reply.Source.IA),
		IP: reply.Source.Host.IP(),
	}
	if src != s.remote || reply.Reply.SeqNumber != expectedSequenceNo {
		return
	}
	pf, err := reversePathFingerprint(reply.Path)
	if err != nil {
		return
	}
	if _, expected := expectedReplies[pf]; !expected {
		return
	}
	stats.RecordLatency(s.remote, pf, reply.RTT())
	delete(expectedReplies, pf)
}

func (s *PingingSelector) Close() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.pinger == nil {
		return nil
	}
	s.pingerCancel()
	return s.pinger.Close()
}

// FabridSelector is a Selector for a single dialed socket.
type FabridSelector struct {
	mutex        sync.Mutex
	paths        []*Path
	current      int
	activePaths  int
	fabridClient *fabrid.Client
	ctx          context.Context
}

func NewFabridSelector(client *fabrid.Client, ctx context.Context) *FabridSelector {
	return &FabridSelector{
		fabridClient: client,
		ctx:          ctx,
	}
}

func (s *FabridSelector) Path() *Path {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if len(s.paths) == 0 {
		return nil
	}
	return s.paths[s.current]
}

func convertInterfaces(interfaces []PathInterface) []snet.PathInterface {
	snetInterfaces := make([]snet.PathInterface, len(interfaces))
	for i, pathInterface := range interfaces {
		snetInterfaces[i] = snet.PathInterface{
			ID: common.IFIDType(pathInterface.IfID),
			IA: addr.IA(pathInterface.IA),
		}
	}
	return snetInterfaces
}

func (s *FabridSelector) Initialize(local, remote UDPAddr, paths []*Path) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	path := paths[0]
	dataplanePath := path.ForwardingPath.dataplanePath

	ifaces := path.Metadata.Interfaces
	hops := make([]snet.FabridPolicyPerHop, 0, len(ifaces)/2+1)

	hops = append(hops, snet.FabridPolicyPerHop{
		Pol:    &s.fabridClient.Config.Policy,
		IA:     addr.IA(ifaces[0].IA),
		Egress: uint16(ifaces[0].IfID),
	})

	for i := 1; i < len(ifaces)-1; i += 2 {
		hops = append(hops, snet.FabridPolicyPerHop{
			Pol:     &s.fabridClient.Config.Policy,
			IA:      addr.IA(ifaces[i].IA),
			Ingress: uint16(ifaces[i].IfID),
			Egress:  uint16(ifaces[i+1].IfID),
		})
	}
	hops = append(hops, snet.FabridPolicyPerHop{
		Pol:     &s.fabridClient.Config.Policy,
		IA:      addr.IA(ifaces[len(ifaces)-1].IA),
		Ingress: uint16(ifaces[len(ifaces)-1].IfID),
	})

	switch previous_path := dataplanePath.(type) {
	case snetpath.SCION:
		fabridConfig := &snetpath.FabridConfig{
			LocalIA:         addr.IA(local.IA),
			LocalAddr:       local.IP.String(),
			DestinationIA:   addr.IA(remote.IA),
			DestinationAddr: remote.IP.String(),
		}
		fabridPath, err := snetpath.NewFABRIDDataplanePath(previous_path, convertInterfaces(path.Metadata.Interfaces),
			hops, fabridConfig, s.fabridClient.NewFabridPathState(snet.PathFingerprint(path.Fingerprint)))
		if err != nil {
			log.Error("Error creating FABRID path", "err", err)
			return
		}
		servicesInfo, err := host().sciond.SVCInfo(s.ctx, []addr.SVC{addr.SvcCS})
		if err != nil {
			log.Error("Error getting services", "err", err)
			return
		}
		controlServiceInfo := servicesInfo[addr.SvcCS][0]
		localAddr := &net.TCPAddr{
			IP:   net.IP(local.IP.AsSlice()),
			Port: 0,
		}
		controlAddr, err := net.ResolveTCPAddr("tcp", controlServiceInfo)
		if err != nil {
			log.Error("Error resolving CS", "err", err)
			return
		}

		log.Debug("Prepared GRPC connection", "CS", controlServiceInfo)
		dialer := func(ctx context.Context, addr string) (net.Conn, error) {
			return net.DialTCP("tcp", localAddr, controlAddr)
		}
		grpcconn, err := grpc.DialContext(s.ctx, controlServiceInfo,
			grpc.WithInsecure(), grpc.WithContextDialer(dialer))
		if err != nil {
			log.Error("Error connection to CS", "err", err)
			return
		}
		client := drpb.NewDRKeyIntraServiceClient(grpcconn)
		fabridPath.RegisterDRKeyFetcher(func(ctx context.Context, meta drkey.ASHostMeta) (drkey.ASHostKey, error) {
			rep, err := client.DRKeyASHost(ctx, drhelper.AsHostMetaToProtoRequest(meta))
			if err != nil {
				return drkey.ASHostKey{}, err
			}
			key, err := drhelper.GetASHostKeyFromReply(rep, meta)
			if err != nil {
				return drkey.ASHostKey{}, err
			}
			return key, nil
		}, func(ctx context.Context, meta drkey.HostHostMeta) (drkey.HostHostKey, error) {
			rep, err := client.DRKeyHostHost(ctx, drhelper.HostHostMetaToProtoRequest(meta))
			if err != nil {
				return drkey.HostHostKey{}, err
			}
			key, err := drhelper.GetHostHostKeyFromReply(rep, meta)
			if err != nil {
				return drkey.HostHostKey{}, err
			}
			return key, nil
		})
		path.ForwardingPath.dataplanePath = fabridPath
	}

	s.paths = paths
	s.current = 0
}

func (s *FabridSelector) Refresh(paths []*Path) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	newcurrent := 0
	if len(s.paths) > 0 {
		currentFingerprint := s.paths[s.current].Fingerprint
		for i, p := range paths {
			if p.Fingerprint == currentFingerprint {
				newcurrent = i
				break
			}
		}
	}
	s.paths = paths
	s.current = newcurrent
}

func (s *FabridSelector) PathDown(pf PathFingerprint, pi PathInterface) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if current := s.paths[s.current]; isInterfaceOnPath(current, pi) || pf == current.Fingerprint {
		fmt.Println("down:", s.current, len(s.paths))
		better := stats.FirstMoreAlive(current, s.paths)
		if better >= 0 {
			// Try next path. Note that this will keep cycling if we get down notifications
			s.current = better
			fmt.Println("failover:", s.current, len(s.paths))
		}
	}
}

func (s *FabridSelector) Close() error {
	return nil
}
