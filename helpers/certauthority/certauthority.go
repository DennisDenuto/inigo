package certauthority

import (
	"io/ioutil"
	"net"
	"path/filepath"
	"time"

	"github.com/square/certstrap/pkix"
)

type CertAuthority interface {
	CAAndKey() (key string, cert string)
	GenerateSelfSignedCertAndKey(string, []string, bool) (key string, cert string, err error)
}

type certAuthority struct {
	depotDir string
	caCert   string
	caKey    string
}

func NewCertAuthority(depotDir, commonName string) (CertAuthority, error) {
	key, cert, err := generateCAAndKey(depotDir, commonName)
	if err != nil {
		return nil, err
	}

	c := certAuthority{
		depotDir: depotDir,
		caCert:   cert,
		caKey:    key,
	}
	return c, nil
}

func (c certAuthority) CAAndKey() (string, string) {
	return c.caKey, c.caCert
}

func (c certAuthority) GenerateSelfSignedCertAndKey(commonName string, sans []string, intermediateCA bool) (string, string, error) {
	key, err := pkix.CreateRSAKey(4096)
	keyBytes, err := key.ExportPrivate()
	if err != nil {
		return handleError(err)
	}

	csr, err := pkix.CreateCertificateSigningRequest(key, "", []net.IP{net.ParseIP("127.0.0.1")}, sans, nil, "", "", "", "", commonName)
	if err != nil {
		return handleError(err)
	}

	caBytes, err := ioutil.ReadFile(c.caCert)
	if err != nil {
		return handleError(err)
	}

	ca, err := pkix.NewCertificateFromPEM(caBytes)
	if err != nil {
		return handleError(err)
	}

	caKeyBytes, err := ioutil.ReadFile(c.caKey)
	if err != nil {
		return handleError(err)
	}

	caKey, err := pkix.NewKeyFromPrivateKeyPEM(caKeyBytes)
	if err != nil {
		return handleError(err)
	}

	var crt *pkix.Certificate
	if intermediateCA {
		crt, err = pkix.CreateIntermediateCertificateAuthority(ca, caKey, csr, time.Now().AddDate(1, 0, 0))
	} else {
		crt, err = pkix.CreateCertificateHost(ca, caKey, csr, time.Now().AddDate(1, 0, 0))
	}
	if err != nil {
		return handleError(err)
	}

	crtBytes, err := crt.Export()
	if err != nil {
		return handleError(err)
	}

	keyFile, err := ioutil.TempFile(c.depotDir, commonName)
	if err != nil {
		return handleError(err)
	}
	err = ioutil.WriteFile(keyFile.Name(), keyBytes, 0655)
	if err != nil {
		return handleError(err)
	}

	crtFile, err := ioutil.TempFile(c.depotDir, commonName)
	if err != nil {
		return handleError(err)
	}
	err = ioutil.WriteFile(crtFile.Name(), crtBytes, 0655)
	if err != nil {
		return handleError(err)
	}

	return keyFile.Name(), crtFile.Name(), nil
}

func generateCAAndKey(depotDir, commonName string) (string, string, error) {
	key, err := pkix.CreateRSAKey(4096)
	if err != nil {
		return handleError(err)
	}

	keyBytes, err := key.ExportPrivate()
	if err != nil {
		return handleError(err)
	}

	crt, err := pkix.CreateCertificateAuthority(key, "", time.Now().AddDate(1, 0, 0), "", "", "", "", commonName)
	if err != nil {
		return handleError(err)
	}

	crtBytes, err := crt.Export()
	if err != nil {
		return handleError(err)
	}

	keyFile := filepath.Join(depotDir, commonName+".key")
	err = ioutil.WriteFile(keyFile, keyBytes, 0655)
	if err != nil {
		return handleError(err)
	}

	crtFile := filepath.Join(depotDir, commonName+".crt")
	err = ioutil.WriteFile(crtFile, crtBytes, 0655)
	if err != nil {
		return handleError(err)
	}

	return keyFile, crtFile, nil
}

func handleError(err error) (string, string, error) {
	return "", "", err
}
