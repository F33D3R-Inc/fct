package runtime

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"net"
	"os"
	"unicode/utf16"

	"golang.org/x/crypto/pkcs12"
)

// ioListenTls implements `listenTls(port: int, identity: text, password:
// text) -> int` (io.net.listen, daemon bodies only, like listen): a
// listener on every interface whose accepted connections speak TLS, the
// server's certificate chain and private key read from a PKCS#12 identity
// file (inside the data sandbox) opened with password — how a server that
// terminates its own TLS is configured (facetql's --tls-identity /
// --tls-identity-password). The handshake happens on a connection's first
// read or write; a failed one reads as a closed connection whose connError
// names why. Everything else about the handle is listen()'s.
func (s *Server) ioListenTls(port int, identity, password string) (any, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("listenTls: invalid port %d (must be 1-65535)", port)
	}
	full, err := s.resolveDataPath(identity)
	if err != nil {
		return nil, fmt.Errorf("listenTls: %w", err)
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("listenTls: failed to read TLS identity file %s: %v", identity, err)
	}
	cert, err := decodePKCS12(raw, password)
	if err != nil {
		return nil, fmt.Errorf("listenTls: failed to load TLS identity: %v", err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("", fmt.Sprint(port)))
	if err != nil {
		// A bind that fails is a value, as it is for listen (netconn.go).
		return s.netConns.mintListener(&netListener{err: osErrorText(err)}), nil
	}
	tl := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	return s.netConns.mintListener(&netListener{ln: tl}), nil
}

// ---- PKCS#12 (RFC 7292) ----
//
// The identity formats OpenSSL writes: PBES2 (PBKDF2 with HMAC-SHA-1/256/
// 384/512, AES-128/192/256-CBC or 3DES-CBC) — OpenSSL 3's default — and the
// legacy pbeWithSHAAnd3-KeyTripleDES-CBC; a file that uses RC2 (OpenSSL 1's
// default for certificates) is read by golang.org/x/crypto/pkcs12, which
// implements RC2. The MAC is verified before anything is decrypted.

var (
	oidData               = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidEncryptedData      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 6}
	oidKeyBag             = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 1}
	oidShroudedKeyBag     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 2}
	oidCertBag            = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 3}
	oidX509Cert           = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 22, 1}
	oidPBES2              = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2             = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidPBEWithSHA3DES     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 3}
	oidPBEWithSHARC2      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 6}
	oidPBEWithSHARC2_128  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 5}
	oidHMACWithSHA1       = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHMACWithSHA256     = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidHMACWithSHA384     = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 10}
	oidHMACWithSHA512     = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 11}
	oidAES128CBC          = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC          = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC          = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
	oidDESEDE3CBC         = asn1.ObjectIdentifier{1, 2, 840, 113549, 3, 7}
	oidSHA1               = asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26}
	oidSHA256             = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidSHA384             = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 2}
	oidSHA512             = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 3}
	errLegacyRC2Encrypted = errors.New("rc2")
)

type p12ContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"tag:0,explicit,optional"`
}

type p12DigestInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	Digest    []byte
}

type p12MacData struct {
	Mac        p12DigestInfo
	MacSalt    []byte
	Iterations int `asn1:"optional,default:1"`
}

type p12PFX struct {
	Version  int
	AuthSafe p12ContentInfo
	MacData  p12MacData `asn1:"optional"`
}

type p12EncryptedContentInfo struct {
	ContentType                asn1.ObjectIdentifier
	ContentEncryptionAlgorithm pkix.AlgorithmIdentifier
	EncryptedContent           []byte `asn1:"tag:0,optional"`
}

type p12EncryptedData struct {
	Version              int
	EncryptedContentInfo p12EncryptedContentInfo
}

type p12SafeBag struct {
	ID         asn1.ObjectIdentifier
	Value      asn1.RawValue   `asn1:"tag:0,explicit"`
	Attributes []asn1.RawValue `asn1:"set,optional"`
}

type p12CertBag struct {
	ID   asn1.ObjectIdentifier
	Data []byte `asn1:"tag:0,explicit"`
}

type p12EncryptedPrivateKeyInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	Data      []byte
}

type p12PBES2Params struct {
	KeyDerivationFunc pkix.AlgorithmIdentifier
	EncryptionScheme  pkix.AlgorithmIdentifier
}

type p12PBKDF2Params struct {
	Salt      []byte
	Iteration int
	KeyLength int                      `asn1:"optional"`
	PRF       pkix.AlgorithmIdentifier `asn1:"optional"`
}

type p12PBEParams struct {
	Salt       []byte
	Iterations int
}

// decodePKCS12 reads an identity: the certificate whose key the file holds,
// then the rest of the chain.
func decodePKCS12(data []byte, password string) (tls.Certificate, error) {
	cert, err := decodePKCS12Native(data, password)
	if errors.Is(err, errLegacyRC2Encrypted) {
		return decodePKCS12Legacy(data, password)
	}
	return cert, err
}

func decodePKCS12Native(data []byte, password string) (tls.Certificate, error) {
	var pfx p12PFX
	rest, err := asn1.Unmarshal(data, &pfx)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("not a PKCS#12 file: %v", err)
	}
	if len(rest) != 0 {
		return tls.Certificate{}, errors.New("not a PKCS#12 file: trailing data")
	}
	if pfx.Version != 3 {
		return tls.Certificate{}, fmt.Errorf("unsupported PKCS#12 version %d", pfx.Version)
	}
	if !pfx.AuthSafe.ContentType.Equal(oidData) {
		return tls.Certificate{}, errors.New("PKCS#12 authenticated safe is not plain data (public-key integrity mode is not supported)")
	}
	var authSafe []byte
	if _, err := asn1.Unmarshal(pfx.AuthSafe.Content.Bytes, &authSafe); err != nil {
		return tls.Certificate{}, fmt.Errorf("PKCS#12 authenticated safe: %v", err)
	}
	if len(pfx.MacData.Mac.Algorithm.Algorithm) > 0 {
		if err := p12VerifyMac(&pfx.MacData, authSafe, password); err != nil {
			return tls.Certificate{}, err
		}
	}
	var contents []p12ContentInfo
	if _, err := asn1.Unmarshal(authSafe, &contents); err != nil {
		return tls.Certificate{}, fmt.Errorf("PKCS#12 contents: %v", err)
	}
	var certs []*x509.Certificate
	var key crypto.PrivateKey
	for _, ci := range contents {
		var safe []byte
		switch {
		case ci.ContentType.Equal(oidData):
			if _, err := asn1.Unmarshal(ci.Content.Bytes, &safe); err != nil {
				return tls.Certificate{}, fmt.Errorf("PKCS#12 safe contents: %v", err)
			}
		case ci.ContentType.Equal(oidEncryptedData):
			var ed p12EncryptedData
			if _, err := asn1.Unmarshal(ci.Content.Bytes, &ed); err != nil {
				return tls.Certificate{}, fmt.Errorf("PKCS#12 encrypted data: %v", err)
			}
			safe, err = p12Decrypt(ed.EncryptedContentInfo.ContentEncryptionAlgorithm, ed.EncryptedContentInfo.EncryptedContent, password)
			if err != nil {
				return tls.Certificate{}, err
			}
		default:
			return tls.Certificate{}, fmt.Errorf("unsupported PKCS#12 content type %v", ci.ContentType)
		}
		var bags []p12SafeBag
		if _, err := asn1.Unmarshal(safe, &bags); err != nil {
			return tls.Certificate{}, fmt.Errorf("PKCS#12 bags (wrong password?): %v", err)
		}
		for _, bag := range bags {
			switch {
			case bag.ID.Equal(oidCertBag):
				var cb p12CertBag
				if _, err := asn1.Unmarshal(bag.Value.Bytes, &cb); err != nil {
					return tls.Certificate{}, fmt.Errorf("PKCS#12 certificate bag: %v", err)
				}
				if !cb.ID.Equal(oidX509Cert) {
					continue
				}
				c, err := x509.ParseCertificate(cb.Data)
				if err != nil {
					return tls.Certificate{}, fmt.Errorf("PKCS#12 certificate: %v", err)
				}
				certs = append(certs, c)
			case bag.ID.Equal(oidShroudedKeyBag):
				var epki p12EncryptedPrivateKeyInfo
				if _, err := asn1.Unmarshal(bag.Value.Bytes, &epki); err != nil {
					return tls.Certificate{}, fmt.Errorf("PKCS#12 key bag: %v", err)
				}
				der, err := p12Decrypt(epki.Algorithm, epki.Data, password)
				if err != nil {
					return tls.Certificate{}, err
				}
				if key, err = x509.ParsePKCS8PrivateKey(der); err != nil {
					return tls.Certificate{}, fmt.Errorf("PKCS#12 private key (wrong password?): %v", err)
				}
			case bag.ID.Equal(oidKeyBag):
				if key, err = x509.ParsePKCS8PrivateKey(bag.Value.Bytes); err != nil {
					return tls.Certificate{}, fmt.Errorf("PKCS#12 private key: %v", err)
				}
			}
		}
	}
	return p12Identity(key, certs)
}

// p12Identity orders the chain leaf-first: the certificate whose public key
// is the private key's.
func p12Identity(key crypto.PrivateKey, certs []*x509.Certificate) (tls.Certificate, error) {
	if key == nil {
		return tls.Certificate{}, errors.New("the PKCS#12 file holds no private key")
	}
	pub, ok := key.(interface{ Public() crypto.PublicKey })
	if !ok {
		return tls.Certificate{}, errors.New("unsupported private key type")
	}
	leaf := -1
	for i, c := range certs {
		if eq, ok := c.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); ok && eq.Equal(pub.Public()) {
			leaf = i
			break
		}
	}
	if leaf < 0 {
		return tls.Certificate{}, errors.New("the PKCS#12 file holds no certificate for its private key")
	}
	out := tls.Certificate{PrivateKey: key, Leaf: certs[leaf]}
	out.Certificate = append(out.Certificate, certs[leaf].Raw)
	for i, c := range certs {
		if i != leaf {
			out.Certificate = append(out.Certificate, c.Raw)
		}
	}
	switch key.(type) {
	case *rsa.PrivateKey, *ecdsa.PrivateKey, ed25519.PrivateKey:
	default:
		return tls.Certificate{}, errors.New("unsupported private key type")
	}
	return out, nil
}

// decodePKCS12Legacy: an RC2-protected file, through x/crypto/pkcs12.
func decodePKCS12Legacy(data []byte, password string) (tls.Certificate, error) {
	blocks, err := pkcs12.ToPEM(data, password)
	if err != nil {
		return tls.Certificate{}, err
	}
	var certs []*x509.Certificate
	var key crypto.PrivateKey
	for _, b := range blocks {
		switch b.Type {
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return tls.Certificate{}, err
			}
			certs = append(certs, c)
		case "PRIVATE KEY":
			if k, err := x509.ParsePKCS8PrivateKey(b.Bytes); err == nil {
				key = k
			} else if k, err := x509.ParsePKCS1PrivateKey(b.Bytes); err == nil {
				key = k
			} else if k, err := x509.ParseECPrivateKey(b.Bytes); err == nil {
				key = k
			} else {
				return tls.Certificate{}, fmt.Errorf("PKCS#12 private key: %v", err)
			}
		}
	}
	return p12Identity(key, certs)
}

func p12Hash(oid asn1.ObjectIdentifier) (func() hash.Hash, int, error) {
	switch {
	case oid.Equal(oidSHA1), oid.Equal(oidHMACWithSHA1):
		return sha1.New, 64, nil
	case oid.Equal(oidSHA256), oid.Equal(oidHMACWithSHA256):
		return sha256.New, 64, nil
	case oid.Equal(oidSHA384), oid.Equal(oidHMACWithSHA384):
		return sha512.New384, 128, nil
	case oid.Equal(oidSHA512), oid.Equal(oidHMACWithSHA512):
		return sha512.New, 128, nil
	}
	return nil, 0, fmt.Errorf("unsupported PKCS#12 digest %v", oid)
}

func p12VerifyMac(md *p12MacData, content []byte, password string) error {
	h, v, err := p12Hash(md.Mac.Algorithm.Algorithm)
	if err != nil {
		return err
	}
	size := h().Size()
	key := p12KDF(h, v, p12BMP(password), md.MacSalt, 3, md.Iterations, size)
	mac := hmac.New(h, key)
	mac.Write(content)
	if !hmac.Equal(mac.Sum(nil), md.Mac.Digest) {
		return errors.New("PKCS#12 MAC verification failed: wrong password, or the file is corrupt")
	}
	return nil
}

// p12BMP: the password as a BMPString with its two-byte terminator.
func p12BMP(password string) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(password)) {
		out = append(out, byte(u>>8), byte(u))
	}
	return append(out, 0, 0)
}

// p12KDF: RFC 7292 appendix B.2.
func p12KDF(h func() hash.Hash, v int, password, salt []byte, id byte, iterations, size int) []byte {
	fill := func(src []byte) []byte {
		if len(src) == 0 {
			return nil
		}
		n := v * ((len(src) + v - 1) / v)
		out := make([]byte, n)
		for i := range out {
			out[i] = src[i%len(src)]
		}
		return out
	}
	D := bytes.Repeat([]byte{id}, v)
	I := append(fill(salt), fill(password)...)
	var out []byte
	one := big.NewInt(1)
	for len(out) < size {
		d := h()
		d.Write(D)
		d.Write(I)
		A := d.Sum(nil)
		for i := 1; i < iterations; i++ {
			d = h()
			d.Write(A)
			A = d.Sum(nil)
		}
		out = append(out, A...)
		B := new(big.Int).SetBytes(fill(A)[:v])
		B.Add(B, one)
		for j := 0; j < len(I); j += v {
			Ij := new(big.Int).SetBytes(I[j : j+v])
			Ij.Add(Ij, B)
			b := Ij.Bytes()
			if len(b) > v {
				b = b[len(b)-v:]
			}
			block := make([]byte, v)
			copy(block[v-len(b):], b)
			copy(I[j:j+v], block)
		}
	}
	return out[:size]
}

func p12Decrypt(alg pkix.AlgorithmIdentifier, data []byte, password string) ([]byte, error) {
	var block cipher.Block
	var iv []byte
	switch {
	case alg.Algorithm.Equal(oidPBES2):
		var params p12PBES2Params
		if _, err := asn1.Unmarshal(alg.Parameters.FullBytes, &params); err != nil {
			return nil, fmt.Errorf("PBES2 parameters: %v", err)
		}
		if !params.KeyDerivationFunc.Algorithm.Equal(oidPBKDF2) {
			return nil, fmt.Errorf("unsupported PBES2 key derivation %v", params.KeyDerivationFunc.Algorithm)
		}
		var kdf p12PBKDF2Params
		if _, err := asn1.Unmarshal(params.KeyDerivationFunc.Parameters.FullBytes, &kdf); err != nil {
			return nil, fmt.Errorf("PBKDF2 parameters: %v", err)
		}
		prf := sha1.New
		if len(kdf.PRF.Algorithm) > 0 {
			h, _, err := p12Hash(kdf.PRF.Algorithm)
			if err != nil {
				return nil, err
			}
			prf = h
		}
		var keyLen int
		es := params.EncryptionScheme
		switch {
		case es.Algorithm.Equal(oidAES128CBC):
			keyLen = 16
		case es.Algorithm.Equal(oidAES192CBC):
			keyLen = 24
		case es.Algorithm.Equal(oidAES256CBC):
			keyLen = 32
		case es.Algorithm.Equal(oidDESEDE3CBC):
			keyLen = 24
		default:
			return nil, fmt.Errorf("unsupported PBES2 cipher %v", es.Algorithm)
		}
		if _, err := asn1.Unmarshal(es.Parameters.FullBytes, &iv); err != nil {
			return nil, fmt.Errorf("PBES2 IV: %v", err)
		}
		key, err := pbkdf2.Key(prf, password, kdf.Salt, kdf.Iteration, keyLen)
		if err != nil {
			return nil, err
		}
		if es.Algorithm.Equal(oidDESEDE3CBC) {
			block, err = des.NewTripleDESCipher(key)
		} else {
			block, err = aes.NewCipher(key)
		}
		if err != nil {
			return nil, err
		}
	case alg.Algorithm.Equal(oidPBEWithSHA3DES):
		var params p12PBEParams
		if _, err := asn1.Unmarshal(alg.Parameters.FullBytes, &params); err != nil {
			return nil, fmt.Errorf("PBE parameters: %v", err)
		}
		pw := p12BMP(password)
		key := p12KDF(sha1.New, 64, pw, params.Salt, 1, params.Iterations, 24)
		iv = p12KDF(sha1.New, 64, pw, params.Salt, 2, params.Iterations, 8)
		var err error
		if block, err = des.NewTripleDESCipher(key); err != nil {
			return nil, err
		}
	case alg.Algorithm.Equal(oidPBEWithSHARC2), alg.Algorithm.Equal(oidPBEWithSHARC2_128):
		return nil, errLegacyRC2Encrypted
	default:
		return nil, fmt.Errorf("unsupported PKCS#12 encryption %v", alg.Algorithm)
	}
	if len(iv) != block.BlockSize() || len(data)%block.BlockSize() != 0 || len(data) == 0 {
		return nil, errors.New("PKCS#12 encrypted content is malformed")
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > block.BlockSize() || pad > len(out) {
		return nil, errors.New("PKCS#12 decryption failed: wrong password, or the file is corrupt")
	}
	for _, b := range out[len(out)-pad:] {
		if int(b) != pad {
			return nil, errors.New("PKCS#12 decryption failed: wrong password, or the file is corrupt")
		}
	}
	return out[:len(out)-pad], nil
}
