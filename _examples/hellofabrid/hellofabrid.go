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
		err = runServer(listen.Get())
		check(err)
	} else {
		err = runClient(*remoteAddr, int(*count))
		check(err)
	}
}

func runServer(listen netip.AddrPort) error {
	conn, err := pan.ListenUDPWithFabrid(context.Background(), listen, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Info("Setup", "local", conn.LocalAddr())

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

func runClient(address string, count int) error {
	log.Info(fmt.Sprintf("Sending %d packets to %s", count, address))
	udpAddr, err := pan.ResolveUDPAddr(context.TODO(), address)
	if err != nil {
		return err
	}
	polIdentifier := snet.FabridPolicyIdentifier{
		Type:       snet.FabridGlobalPolicy,
		Identifier: 0,
		Index:      0,
	}
	fabridConfig := fabrid.SimpleFabridConfig{
		DestinationIA:   addr.IA(udpAddr.IA),
		DestinationAddr: udpAddr.IP.String(),
		ValidationRatio: 255,
		Policy:          polIdentifier,
		//SuccessValFunc: func() {
		//	fmt.Println("Validation for packet ID success")
		//},
		//FailValFunc: func() {
		//	fmt.Println("Validation for packet ID failed")
		//},
	}

	conn, client, err := pan.DialUDPWithFabrid(context.Background(), netip.AddrPort{}, udpAddr, nil, fabridConfig)
	if err != nil {
		return err
	}
	defer conn.Close()

	for i := 0; i < count; i++ {

		if i%10 == 0 {
			client.SetValidationRatio(uint8(255 - 5*i))
		}
		_, err := conn.Write([]byte(fmt.Sprintf("hello fabrid %s", time.Now().Format("15:04:05.0"))))
		if err != nil {
			return err
		}
		//fmt.Printf("Wrote %d bytes.\n", nBytes)

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
