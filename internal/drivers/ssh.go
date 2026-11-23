package drivers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSHShutdown runs a shutdown command over SSH.
type SSHShutdown struct {
	Address    string // host:port
	User       string
	PrivateKey []byte
	KnownHosts []byte
	Insecure   bool
	Command    string
}

func (s *SSHShutdown) Shutdown(ctx context.Context) error {
	signer, err := ssh.ParsePrivateKey(s.PrivateKey)
	if err != nil {
		return fmt.Errorf("ssh private key: %w", err)
	}
	var hostKeyCB ssh.HostKeyCallback
	switch {
	case len(s.KnownHosts) > 0:
		hostKeyCB, err = knownHostsCallback(s.KnownHosts)
		if err != nil {
			return err
		}
	case s.Insecure:
		hostKeyCB = ssh.InsecureIgnoreHostKey() //nolint:gosec // explicitly requested by the user
	default:
		return errors.New("ssh: no knownHosts in secret and insecureIgnoreHostKey is false")
	}
	cfg := &ssh.ClientConfig{
		User:            s.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCB,
		Timeout:         15 * time.Second,
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", s.Address)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, s.Address, cfg)
	if err != nil {
		conn.Close()
		return fmt.Errorf("ssh handshake: %w", err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	err = sess.Run(s.Command)
	// The connection usually drops while the machine shuts down; that's success.
	var exitMissing *ssh.ExitMissingError
	if err == nil || errors.As(err, &exitMissing) {
		return nil
	}
	return fmt.Errorf("ssh %q: %w", s.Command, err)
}

func knownHostsCallback(data []byte) (ssh.HostKeyCallback, error) {
	f, err := writeTemp(data)
	if err != nil {
		return nil, err
	}
	return knownhosts.New(f)
}

func (e *Env) ssh(ctx context.Context, s *v1alpha1.SSH, node *corev1.Node) error {
	data, err := e.Secret(ctx, s.CredentialsSecretRef.Name, s.CredentialsSecretRef.Namespace)
	if err != nil {
		return err
	}
	addr := s.Address
	if addr == "" {
		addr = InternalIP(node)
	}
	if addr == "" {
		return errors.New("ssh: no address and the node has no InternalIP")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "22")
	}
	sh := &SSHShutdown{Address: addr, User: s.User, Command: s.Command, PrivateKey: data["privateKey"],
		KnownHosts: data["knownHosts"], Insecure: s.InsecureIgnoreHostKey}
	if sh.User == "" {
		sh.User = "root"
	}
	if sh.Command == "" {
		sh.Command = "poweroff"
	}
	return sh.Shutdown(ctx)
}

func writeTemp(data []byte) (string, error) {
	f, err := os.CreateTemp("", "known_hosts")
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = f.Write(data)
	return f.Name(), err
}
