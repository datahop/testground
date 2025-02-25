package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/testground/sdk-go/network"
	"github.com/testground/sdk-go/run"
	"github.com/testground/sdk-go/runtime"
	"github.com/testground/sdk-go/sync"
)

func pingpong(runenv *runtime.RunEnv, initCtx *run.InitContext) error {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	runenv.RecordMessage("before sync.MustBoundClient")
	client := initCtx.SyncClient
	netclient := initCtx.NetClient

	oldAddrs, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}

	config := &network.Config{
		// Control the "default" network. At the moment, this is the only network.
		Network: "default",

		// Enable this network. Setting this to false will disconnect this test
		// instance from this network. You probably don't want to do that.
		Enable: true,
		Default: network.LinkShape{
			Latency:   100 * time.Millisecond,
			Bandwidth: 1 << 20, // 1Mib
		},
		CallbackState: "network-configured",
		RoutingPolicy: network.DenyAll,
	}

	runenv.RecordMessage("before netclient.MustConfigureNetwork")
	netclient.MustConfigureNetwork(ctx, config)

	// Make sure that the IP addresses don't change unless we request it.
	if newAddrs, err := net.InterfaceAddrs(); err != nil {
		return err
	} else if !sameAddrs(oldAddrs, newAddrs) {
		return fmt.Errorf("interfaces changed")
	}

	seq := client.MustSignalAndWait(ctx, "ip-allocation", runenv.TestInstanceCount)

	runenv.RecordMessage("I am %d", seq)

	ipC := byte((seq >> 8) + 1)
	ipD := byte(seq)

	config.IPv4 = runenv.TestSubnet
	config.IPv4.IP = append(config.IPv4.IP[0:2:2], ipC, ipD)
	// Translates to /15 mask. The mask needs to match the IP range of the TestSubnet,
	// otherwise some addresses may be excluded, causing the test to fail
	config.IPv4.Mask = []byte{255, 254, 0, 0}
	config.CallbackState = "ip-changed"

	var (
		listener *net.TCPListener
		conn     *net.TCPConn
	)

	if seq == 1 {
		listener, err = net.ListenTCP("tcp4", &net.TCPAddr{Port: 1234})
		if err != nil {
			return err
		}
		defer listener.Close()
	}

	runenv.RecordMessage("before reconfiguring network")
	netclient.MustConfigureNetwork(ctx, config)

	ownDataIp := netclient.MustGetDataNetworkIP()
	peerAddrs, err := getPeerAddrs(ctx, runenv, ownDataIp, client)
	if err != nil {
		return err
	}

	switch seq {
	case 1:
		runenv.RecordMessage("Listening at", "address", listener.Addr())
		conn, err = listener.AcceptTCP()
	case 2:
		addr := strings.Split(peerAddrs[0], ":")[0]
		var targetIp = net.ParseIP(addr)
		runenv.RecordMessage("Attempting to connect to ", "target", targetIp)
		conn, err = net.DialTCP("tcp4", nil, &net.TCPAddr{
			IP:   targetIp,
			Port: 1234,
		})
	default:
		return fmt.Errorf("expected at most two test instances")
	}
	if err != nil {
		return err
	}

	defer conn.Close()

	// trying to measure latency here.
	err = conn.SetNoDelay(true)
	if err != nil {
		return err
	}

	pingPong := func(test string, rttMin, rttMax time.Duration) error {
		buf := make([]byte, 1)

		runenv.RecordMessage("waiting until ready")

		// wait till both sides are ready
		_, err = conn.Write([]byte{0})
		if err != nil {
			return err
		}

		_, err = conn.Read(buf)
		if err != nil {
			return err
		}

		start := time.Now()

		// write sequence number.
		runenv.RecordMessage("writing my id")
		_, err = conn.Write([]byte{byte(seq)})
		if err != nil {
			return err
		}

		// pong other sequence number
		runenv.RecordMessage("reading their id")
		_, err = conn.Read(buf)
		if err != nil {
			return err
		}

		runenv.RecordMessage("returning their id")
		_, err = conn.Write(buf)
		if err != nil {
			return err
		}

		runenv.RecordMessage("reading my id")
		// read our sequence number
		_, err = conn.Read(buf)
		if err != nil {
			return err
		}

		runenv.RecordMessage("done")

		// stop
		end := time.Now()

		// check the sequence number.
		if buf[0] != byte(seq) {
			return fmt.Errorf("read unexpected value")
		}

		// check the RTT
		rtt := end.Sub(start)
		if rtt < rttMin || rtt > rttMax {
			return fmt.Errorf("expected an RTT between %s and %s, got %s", rttMin, rttMax, rtt)
		}
		runenv.RecordMessage("ping RTT was %s [%s, %s]", rtt, rttMin, rttMax)

		// Don't reconfigure the network until we're done with the first test.
		state := sync.State("ping-pong-" + test)
		client.MustSignalAndWait(ctx, state, runenv.TestInstanceCount)

		return nil
	}
	err = pingPong("200", 200*time.Millisecond, 215*time.Millisecond)
	if err != nil {
		return err
	}

	config.Default.Latency = 10 * time.Millisecond
	config.CallbackState = "latency-reduced"
	netclient.MustConfigureNetwork(ctx, config)

	runenv.RecordMessage("ping pong")
	err = pingPong("10", 20*time.Millisecond, 35*time.Millisecond)
	if err != nil {
		return err
	}

	return nil
}

// Returns data network addresses of test peer instances
func getPeerAddrs(ctx context.Context, runenv *runtime.RunEnv, ownDataIp net.IP, client sync.Client) ([]string, error) {
	_ = client.MustSignalAndWait(ctx, sync.State("listening"), runenv.TestInstanceCount)

	peerAddrs, err := exchangeAddrWithPeers(ctx, client, runenv, ownDataIp.String())
	if err != nil {
		return nil, err
	}

	_ = client.MustSignalAndWait(ctx, sync.State("got-other-addrs"), runenv.TestInstanceCount)

	return peerAddrs, nil
}

// Shares this instance's address with all other instances in the test,
//  collects all other instances' addresses and returns them
func exchangeAddrWithPeers(ctx context.Context, client sync.Client, runenv *runtime.RunEnv, addr string) ([]string, error) {
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	peerTopic := sync.NewTopic("peers", "")
	ch := make(chan string)
	if _, _, err := client.PublishSubscribe(subCtx, peerTopic, addr, ch); err != nil {
		return nil, fmt.Errorf(err.Error())
	}

	res := []string{}

	for i := 0; i < runenv.TestInstanceCount; i++ {
		select {
		case otherAddr := <-ch:
			runenv.RecordMessage("got info: %d: %s", i, otherAddr)
			if addr != otherAddr {
				res = append(res, otherAddr)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		runenv.D().Counter("got.info").Inc(1)
	}

	return res, nil
}

func sameAddrs(a, b []net.Addr) bool {
	if len(a) != len(b) {
		return false
	}
	aset := make(map[string]bool, len(a))
	for _, addr := range a {
		aset[addr.String()] = true
	}
	for _, addr := range b {
		if !aset[addr.String()] {
			return false
		}
	}
	return true
}
