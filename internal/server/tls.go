package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

// tlsManager termina TLS del lado GPM (la capa "stunnel embebido"): envuelve
// la conexión cruda en TLS 1.3 con SNI antes de que corra la detección de
// señuelo / handshake SSH (ver handleConn). El cliente conecta con
// allowInsecure (no valida la cadena), así que NO hace falta un cert real de
// una CA pública ni un dominio propio: GPM firma sus propios leaf certs con
// una CA self-signed interna.
//
// IMPORTANTE: con allowInsecure + cert self-signed esta capa TLS es
// CAMUFLAJE, no seguridad -- no autentica al servidor. El cifrado y la
// autenticación reales los da el SSH de adentro (el uuid como credencial).
//
// Por qué un leaf DISTINTO por SNI (generado al vuelo y cacheado) en vez de
// un cert fijo: el ServerName del ClientHello viaja en claro; si un DPI lo
// compara contra el CN/SAN del certificado, así coinciden y el nodo parece
// un HTTPS legítimo de ESE dominio. En TLS 1.3 el cert va cifrado y esto
// pesa menos, pero el costo es cero y cubre TLS 1.2 y sondeos activos. La CA
// se persiste (identidad estable entre reinicios); los leaf se cachean en
// memoria y se regeneran al arrancar (al cliente le da igual, no valida).
type tlsManager struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caDER  []byte

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

func newTLSManager(caPath string) (*tlsManager, error) {
	caCert, caKey, caDER, err := loadOrCreateCA(caPath)
	if err != nil {
		return nil, err
	}
	return &tlsManager{
		caCert: caCert,
		caKey:  caKey,
		caDER:  caDER,
		cache:  map[string]*tls.Certificate{},
	}, nil
}

// serverConfig arma el *tls.Config para tls.Server: TLS 1.2 como piso (deja
// caer a 1.2 a un cliente viejo, pero prefiere 1.3), sin Certificates fijos
// -- todo pasa por GetCertificate, que emite/cachea un leaf por SNI.
func (m *tlsManager) serverConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: m.getCertificate,
	}
}

func (m *tlsManager) getCertificate(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := h.ServerName
	if name == "" {
		// Un cliente que no manda SNI (raro con el nuestro, pero posible con
		// un sondeo) igual tiene que recibir un cert válido para completar el
		// handshake.
		name = "localhost"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cache[name]; ok {
		return c, nil
	}
	cert, err := m.mintLeaf(name)
	if err != nil {
		return nil, err
	}
	m.cache[name] = cert
	return cert, nil
}

func (m *tlsManager) mintLeaf(name string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(name); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{name}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.caCert, &key.PublicKey, m.caKey)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		// Mandamos leaf + CA para que la cadena que ve el cliente sea de dos
		// niveles (más parecida a un HTTPS real); igual el cliente no la
		// valida (allowInsecure).
		Certificate: [][]byte{der, m.caDER},
		PrivateKey:  key,
		Leaf:        tmpl,
	}, nil
}

// loadOrCreateCA carga (o crea y persiste) la CA self-signed que firma los
// leaf por SNI. Mismo espíritu que loadOrCreateHostKey: si path existe la
// lee, si no la genera y guarda; path vacío = CA efímera (nueva en cada
// arranque, suficiente porque el cliente no valida la cadena). El archivo
// trae dos bloques PEM: el cert de la CA y su clave EC.
func loadOrCreateCA(path string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			return parseCA(data, path)
		}
	}
	cert, key, der, pemBytes, err := generateCA()
	if err != nil {
		return nil, nil, nil, err
	}
	if path != "" {
		if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
			return nil, nil, nil, fmt.Errorf("guardar %s: %w", path, err)
		}
		log.Println("CA TLS nueva generada y guardada en", path)
	}
	return cert, key, der, nil
}

func parseCA(data []byte, path string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	var certDER, keyDER []byte
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case "CERTIFICATE":
			certDER = block.Bytes
		case "EC PRIVATE KEY":
			keyDER = block.Bytes
		}
	}
	if certDER == nil || keyDER == nil {
		return nil, nil, nil, fmt.Errorf("%s: falta el bloque CERTIFICATE o EC PRIVATE KEY", path)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	key, err := x509.ParseECPrivateKey(keyDER)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return cert, key, certDER, nil
}

func generateCA() (cert *x509.Certificate, key *ecdsa.PrivateKey, der []byte, pemBytes []byte, err error) {
	key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "GPM Root"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	pemBytes = append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...,
	)
	return cert, key, der, pemBytes, nil
}
