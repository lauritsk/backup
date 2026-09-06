package imap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/lauritsk/backup/internal/pimbackup/config"
)

func TestNetworkDialerTLSModes(t *testing.T) {
	for _, mode := range []string{"implicit", "starttls"} {
		t.Run(mode, func(t *testing.T) {
			address, closeServer := startTLSTestServer(t, mode == "implicit")
			defer closeServer()
			host, portText, err := net.SplitHostPort(address)
			if err != nil {
				t.Fatal(err)
			}
			port, err := net.LookupPort("tcp", portText)
			if err != nil {
				t.Fatal(err)
			}
			account := config.AccountConfig{
				ID:                 "test",
				Host:               host,
				Port:               port,
				TLS:                mode,
				InsecureSkipVerify: true,
				AllowInsecure:      true,
				Username:           "user",
				ResolvedPassword:   "password",
				Timeout:            config.Duration{Duration: 5 * time.Second},
			}
			remote, err := (NetworkDialer{}).Dial(context.Background(), account)
			if err != nil {
				t.Fatal(err)
			}
			mailboxes, err := remote.ListMailboxes(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(mailboxes) != 1 || mailboxes[0].Name != "INBOX" {
				t.Fatalf("mailboxes = %#v", mailboxes)
			}
			if err := remote.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNetworkDialerAuthenticationTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = (NetworkDialer{}).Dial(context.Background(), config.AccountConfig{
		ID: "timeout", Host: host, Port: port, TLS: "plain", AllowInsecure: true, Username: "user", ResolvedPassword: "password",
		Timeout: config.Duration{Duration: 100 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("Dial() succeeded against a server that sent no greeting")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Dial() took %s despite its authentication timeout", elapsed)
	}
	select {
	case connection := <-accepted:
		connection.Close()
	default:
	}
}

func startTLSTestServer(t *testing.T, implicit bool) (string, func()) {
	t.Helper()
	memoryServer := imapmemserver.New()
	user := imapmemserver.NewUser("user", "password")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	memoryServer.AddUser(user)
	certificate := testCertificate(t)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memoryServer.NewSession(), nil, nil
		},
		TLSConfig: tlsConfig,
		Caps:      goimap.CapSet{goimap.CapIMAP4rev1: {}},
	})
	var listener net.Listener
	var err error
	if implicit {
		listener, err = tls.Listen("tcp", "127.0.0.1:0", tlsConfig)
	} else {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	return listener.Addr().String(), func() {
		if err := server.Close(); err != nil {
			t.Errorf("server.Close() = %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("server.Serve() = %v", err)
		}
	}
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
