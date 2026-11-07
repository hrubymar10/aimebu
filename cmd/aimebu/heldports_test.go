package main

import (
	"net"
	"strconv"
	"sync"
	"testing"
)

// A test that starts the server needs a port for it. Finding one by listening on :0, closing and passing the number on leaves a gap in which
// anything else on the machine (another test binary of a parallel run, the dev server's ephemeral connection) can take the port, and the
// server then fails to start with "already in use". These helpers close the gap: the test keeps its listener open and the server, which asks
// for the port through listenTCP, is handed that very listener (listenOverride). The port is never released.
//
// Only an in-process server (serverCommand, run) can be handed a listener. A detached server that a test starts as a process (daemonFixture)
// binds on its own, so that fixture still has a short gap.

var held struct {
	sync.Mutex
	ports map[int]net.Listener
}

// holdFreePorts binds n distinct ports on 127.0.0.1 and keeps them bound until the server under test listens on each of them, or until the
// test ends. All n are open at once, so no port is picked twice.
func holdFreePorts(t *testing.T, n int) []int {
	t.Helper()
	held.Lock()
	defer held.Unlock()
	if held.ports == nil {
		held.ports = map[int]net.Listener{}
	}
	ports := make([]int, 0, n)
	for range n {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		held.ports[port] = listener
		ports = append(ports, port)
	}
	hook := heldListen
	listenOverride.Store(&hook)
	t.Cleanup(func() {
		held.Lock()
		defer held.Unlock()
		for _, port := range ports {
			if listener, ok := held.ports[port]; ok {
				listener.Close()
				delete(held.ports, port)
			}
		}
		if len(held.ports) == 0 {
			listenOverride.Store(nil)
		}
	})
	return ports
}

// holdPort holds one port, makes it the server's port (AIMEBU2_PORT) and returns it.
func holdPort(t *testing.T) int {
	t.Helper()
	port := holdFreePorts(t, 1)[0]
	t.Setenv("AIMEBU2_PORT", strconv.Itoa(port))
	return port
}

// heldListen is listenOverride while a port is held: it hands over the open listener for a held loopback port, once. A bind to the same port a
// second time (a restart) or on another address goes to the operating system as usual; for another address the held port is released first,
// as it is not the one that was bound.
func heldListen(address string) (net.Listener, error) {
	host, portText, err := net.SplitHostPort(address)
	if err == nil {
		if port, perr := strconv.Atoi(portText); perr == nil {
			held.Lock()
			listener, ok := held.ports[port]
			if ok {
				delete(held.ports, port)
			}
			held.Unlock()
			if ok && host == "127.0.0.1" {
				return listener, nil
			}
			if ok {
				listener.Close()
			}
		}
	}
	return net.Listen("tcp", address)
}

// A held port is not free for anyone else until the server under test takes it, and then it is that very socket.
func TestAHeldPortIsHandedToTheServerAndNeverReleasedBeforeThat(t *testing.T) {
	ports := holdFreePorts(t, 2)
	if ports[0] == ports[1] {
		t.Fatalf("one port was held twice: %v", ports)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0]))
	if stranger, err := net.Listen("tcp", address); err == nil {
		stranger.Close()
		t.Fatalf("another listener could take %s while it was held", address)
	}
	listener, err := listenTCP(address)
	if err != nil {
		t.Fatal(err)
	}
	if listener.Addr().String() != address {
		t.Fatalf("handed over %s, want %s", listener.Addr(), address)
	}
	if stranger, err := net.Listen("tcp", address); err == nil {
		stranger.Close()
		t.Fatalf("another listener could take %s once the server had it", address)
	}
	// The second held port is untouched by the first hand-over.
	other := net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[1]))
	if stranger, err := net.Listen("tcp", other); err == nil {
		stranger.Close()
		t.Fatalf("%s was released with the first", other)
	}
	// Once the server lets go (a stop, then a start again) the port is an ordinary free port: the next bind goes to the system.
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := listenTCP(address)
	if err != nil {
		t.Fatalf("a second bind after the first was closed: %v", err)
	}
	again.Close()
}

// Asked for on another address, a held port is released first, so the bind does not collide with the test's own socket.
func TestAHeldPortAskedForOnAnotherAddressIsReleasedFirst(t *testing.T) {
	port := holdFreePorts(t, 1)[0]
	listener, err := listenTCP(net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("a wildcard bind over a held loopback port: %v", err)
	}
	listener.Close()
}

// Without a held port the override is off and listenTCP is net.Listen.
func TestListenTCPIsPlainListenWhenNothingIsHeld(t *testing.T) {
	if hook := listenOverride.Load(); hook != nil {
		t.Fatal("a hook is installed with no port held")
	}
	listener, err := listenTCP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
}
