// Copyright 2018 ETH Zurich
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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/scionproto/scion/pkg/addr"
	"github.com/scionproto/scion/pkg/experimental/fabrid"
	"github.com/scionproto/scion/pkg/log"
	"github.com/scionproto/scion/pkg/private/serrors"
	"github.com/scionproto/scion/pkg/slayers/extension"
	"github.com/scionproto/scion/pkg/snet"
	"net/netip"
	"os"
	"time"

	"github.com/netsec-ethz/scion-apps/pkg/pan"
)

func main() {
	var err error
	// get local and remote addresses from program arguments:
	var listen pan.IPPortValue
	var logConsole string
	flag.Var(&listen, "listen", "[Server] local IP:port to listen on")
	remoteAddr := flag.String("remote", "", "[Client] Remote (i.e. the server's) SCION Address (e.g. 17-ffaa:1:1,[127.0.0.1]:12345)")
	count := flag.Uint("count", 1, "[Client] Number of messages to send")
	maxValRatio := flag.Uint("max-val-ratio", 255, "[Server] Maximum allowed validation ratio")
	valRatio := flag.Uint("val-ratio", 0, "[Client] Requested validation ratio")
	flag.StringVar(&logConsole, "log.console", "info", "Console logging level: debug|info|error")
	flag.Parse()

	logCfg := log.Config{Console: log.ConsoleConfig{Level: logConsole, StacktraceLevel: "none"}}
	if err := log.Setup(logCfg); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s", err)
		flag.Usage()
		os.Exit(1)
	}
	if (listen.Get().Port() > 0) == (len(*remoteAddr) > 0) {
		check(fmt.Errorf("either specify -listen for server or -remote for client"))
	}

	if listen.Get().Port() > 0 {
		err = runServer(listen.Get(), int(*maxValRatio))
		check(err)
	} else {
		err = runClient(*remoteAddr, int(*count), int(*valRatio))
		check(err)
	}
}

// STRICT immediately aborts connection if either server or client validation fails
const STRICT bool = false

func runServer(listen netip.AddrPort, maxValRatio int) error {

	// Open a new UDP listen connection using FABRID, which returns a FABRID server instance
	conn, server, err := pan.ListenUDPWithFabrid(context.Background(), listen, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Info("Setup", "local", conn.LocalAddr(), "maxValRatio", maxValRatio)

	// Define the server behaviour for validation events
	server.ValidationHandler = func(connection *fabrid.ClientConnection, id *extension.IdentifierOption, success bool) error {
		if success {
			log.Info("Successful validation", "packetID", id.PacketID, "timestamp", id.Timestamp)
		} else {
			log.Info("Failed validation", "packetID", id.PacketID, "timestamp", id.Timestamp)
			connection.Stats.InvalidPackets++
			if STRICT {
				return serrors.New("Aborting on failed validation")
			}
		}
		return nil
	}
	server.MaxValidationRatio = uint8(maxValRatio)

	buffer := make([]byte, 16*1024)
	for {
		n, from, err := conn.ReadFrom(buffer)
		if err != nil {
			return err
		}
		data := buffer[:n]
		log.Info(fmt.Sprintf("Received %s: %s", from, data))
		msg := fmt.Sprintf("take it back! %s", time.Now().Format("15:04:05.0"))
		n, err = conn.WriteTo([]byte(msg), from)
		if err != nil {
			return err
		}
		log.Debug(fmt.Sprintf("Wrote %d bytes.", n))
	}
}

func runClient(address string, count int, valRatio int) error {
	log.Info(fmt.Sprintf("Sending %d packets to %s", count, address))
	udpAddr, err := pan.ResolveUDPAddr(context.TODO(), address)
	if err != nil {
		return err
	}

	// Specify FABRID policy (here the ZERO policy is used to just do path validation)
	polIdentifier := snet.FabridPolicyIdentifier{
		Type:       snet.FabridGlobalPolicy,
		Identifier: 0,
		Index:      0,
	}

	// Create the FABRID configuration, including the validation handler for at source validation
	fabridConfig := fabrid.SimpleFabridConfig{
		DestinationIA:   addr.IA(udpAddr.IA),
		DestinationAddr: udpAddr.IP.String(),
		ValidationRatio: uint8(valRatio),
		Policy:          polIdentifier,
		ValidationHandler: func(pathState *fabrid.PathState, fc *extension.FabridControlOption, success bool) error {
			if success {
				log.Info("Successful validation", "packetID", fc.PacketID, "timestamp", fc.Timestamp)
			} else {
				log.Info("Failed validation", "packetID", fc.PacketID, "timestamp", fc.Timestamp)
				pathState.Stats.InvalidPackets++

				// Example behaviour, increase validation ratio if previous validations have failed
				newValRatio := uint16(pathState.ValidationRatio) * 2
				if newValRatio > 255 {
					newValRatio = 255
				}
				pathState.ValidationRatio = uint8(newValRatio)
				pathState.UpdateValRatio = true
				if STRICT {
					return serrors.New("Aborting on failed validation")
				}
			}
			return nil
		},
	}

	// Open a new UDP connection using FABRID, which returns a FABRID client instance
	conn, client, err := pan.DialUDPWithFabrid(context.Background(), netip.AddrPort{}, udpAddr, nil, fabridConfig)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Send `count` number of packets to the destination
	for i := 0; i < count; i++ {

		// Every 10th packet, request statistics from the destination
		if i%10 == 9 {
			client.RequestStatistics()
		}

		_, err := conn.Write([]byte(fmt.Sprintf("hello fabrid %s", time.Now().Format("15:04:05.0"))))
		if err != nil {
			return err
		}

		buffer := make([]byte, 16*1024)
		if err = conn.SetReadDeadline(time.Now().Add(1 * time.Second)); err != nil {
			return err
		}
		n, err := conn.Read(buffer)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		} else if err != nil {
			return err
		}
		data := buffer[:n]
		log.Info(fmt.Sprintf("Received reply: %s", data))
	}
	return nil
}

// Check just ensures the error is nil, or complains and quits
func check(e error) {
	if e != nil {
		fmt.Fprintln(os.Stderr, "Fatal error:", e)
		os.Exit(1)
	}
}
