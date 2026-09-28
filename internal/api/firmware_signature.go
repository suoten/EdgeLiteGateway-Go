package api

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
)

// ============================================================================
// Firmware signature: real signing (Ed25519/ECDSA/RSA/HMAC) with JSON-backed
// storage under <db dir>/firmware.
// ============================================================================

type firmwareSigRecord struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	Version   string    `json:"version"`
	Size      int64     `json:"size"`
	Algorithm string    `json:"algorithm"`
	Signature string    `json:"signature"`
	PublicKey string    `json:"public_key,omitempty"`
	SignedAt  time.Time `json:"signed_at"`
	Verified  bool      `json:"verified"`
	VerifyKey string    `json:"-"`
	FilePath  string    `json:"-"`
}

type firmwareSigStore struct {
	mu      sync.Mutex
	records map[string]*firmwareSigRecord
	path    string
}

var fwSigStore *firmwareSigStore
var fwSigOnce sync.Once

func getFirmwareSigStore() *firmwareSigStore {
	fwSigOnce.Do(func() {
		dbPath := config.GetConfig().Database.SQLitePath
		dir := filepath.Join(filepath.Dir(dbPath), "firmware")
		fwSigStore = &firmwareSigStore{
			records: map[string]*firmwareSigRecord{},
			path:    filepath.Join(dir, "signatures.json"),
		}
		if data, err := os.ReadFile(fwSigStore.path); err == nil {
			var list []*firmwareSigRecord
			if json.Unmarshal(data, &list) == nil {
				for _, r := range list {
					fwSigStore.records[r.ID] = r
				}
			}
		}
	})
	return fwSigStore
}

func (s *firmwareSigStore) dir() string {
	return filepath.Dir(s.path)
}

func (s *firmwareSigStore) saveLocked() {
	list := make([]*firmwareSigRecord, 0, len(s.records))
	for _, r := range s.records {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].SignedAt.After(list[j].SignedAt) })
	if data, err := json.MarshalIndent(list, "", "  "); err == nil {
		if err := os.MkdirAll(s.dir(), 0o755); err == nil {
			_ = os.WriteFile(s.path, data, 0o600)
		}
	}
}

// asymmetricKeyCache caches generated asymmetric keys per passphrase so
// repeated signs with the same key skip expensive keygen. Verification uses
// the stored public key and never needs this cache.
var (
	asymKeyMu    sync.Mutex
	asymKeyCache = map[string]interface{}{}
)

func cachedAsymKey(algorithm, passphrase string, gen func() (interface{}, error)) (interface{}, error) {
	k := algorithm + "|" + passphrase
	asymKeyMu.Lock()
	defer asymKeyMu.Unlock()
	if v, ok := asymKeyCache[k]; ok {
		return v, nil
	}
	v, err := gen()
	if err != nil {
		return nil, err
	}
	asymKeyCache[k] = v
	return v, nil
}

func algorithmNameNormalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// signFirmwareContent signs content with the requested algorithm. For HMAC the
// key is used directly; for asymmetric algorithms the passphrase deterministically
// derives the private key so signing is reproducible.
func signFirmwareContent(algorithm, signingKey string, content []byte) (signature, publicKey, verifyKey string, err error) {
	switch algorithmNameNormalize(algorithm) {
	case "hmac256":
		mac := hmac.New(sha256.New, []byte(signingKey))
		mac.Write(content)
		return hex.EncodeToString(mac.Sum(nil)), "", signingKey, nil
	case "ed25519":
		seed := sha256.Sum256([]byte("edgelite-firmware-sign:" + signingKey))
		priv := ed25519.NewKeyFromSeed(seed[:])
		pub, _ := priv.Public().(ed25519.PublicKey)
		sig := ed25519.Sign(priv, content)
		return hex.EncodeToString(sig), hex.EncodeToString(pub), "", nil
	case "ecdsa256":
		privAny, err := cachedAsymKey("ecdsa256", signingKey, func() (interface{}, error) {
			return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		})
		if err != nil {
			return "", "", "", err
		}
		priv := privAny.(*ecdsa.PrivateKey)
		digest := sha256.Sum256(content)
		sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
		if err != nil {
			return "", "", "", err
		}
		pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		if err != nil {
			return "", "", "", err
		}
		return hex.EncodeToString(sig), hex.EncodeToString(pubDER), "", nil
	case "rsa2048":
		privAny, err := cachedAsymKey("rsa2048", signingKey, func() (interface{}, error) {
			return rsa.GenerateKey(rand.Reader, 2048)
		})
		if err != nil {
			return "", "", "", err
		}
		priv := privAny.(*rsa.PrivateKey)
		digest := sha256.Sum256(content)
		sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
		if err != nil {
			return "", "", "", err
		}
		pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		if err != nil {
			return "", "", "", err
		}
		return hex.EncodeToString(sig), hex.EncodeToString(pubDER), "", nil
	default:
		return "", "", "", fmt.Errorf("unsupported algorithm: %s", algorithm)
	}
}

func verifyFirmwareContent(rec *firmwareSigRecord, content []byte) (bool, error) {
	sig, err := hex.DecodeString(rec.Signature)
	if err != nil {
		return false, err
	}
	switch rec.Algorithm {
	case "hmac256":
		mac := hmac.New(sha256.New, []byte(rec.VerifyKey))
		mac.Write(content)
		return hmac.Equal(sig, mac.Sum(nil)), nil
	case "ed25519":
		pub, err := hex.DecodeString(rec.PublicKey)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return false, errors.New("invalid public key")
		}
		return ed25519.Verify(ed25519.PublicKey(pub), content, sig), nil
	case "ecdsa256":
		pubDER, err := hex.DecodeString(rec.PublicKey)
		if err != nil {
			return false, err
		}
		pubAny, err := x509.ParsePKIXPublicKey(pubDER)
		if err != nil {
			return false, err
		}
		pub, ok := pubAny.(*ecdsa.PublicKey)
		if !ok {
			return false, errors.New("invalid public key type")
		}
		digest := sha256.Sum256(content)
		return ecdsa.VerifyASN1(pub, digest[:], sig), nil
	case "rsa2048":
		pubDER, err := hex.DecodeString(rec.PublicKey)
		if err != nil {
			return false, err
		}
		pubAny, err := x509.ParsePKIXPublicKey(pubDER)
		if err != nil {
			return false, err
		}
		pub, ok := pubAny.(*rsa.PublicKey)
		if !ok {
			return false, errors.New("invalid public key type")
		}
		digest := sha256.Sum256(content)
		return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) == nil, nil
	default:
		return false, fmt.Errorf("unsupported algorithm: %s", rec.Algorithm)
	}
}

func firmwareFilePath(filename string) (string, error) {
	name := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	if name == "" || name == "." || strings.Contains(name, "..") {
		return "", errors.New("invalid file name")
	}
	return filepath.Join(getFirmwareSigStore().dir(), name), nil
}

func handleSignFirmware(c echo.Context) error {
	var req struct {
		FilePath    string `json:"file_path"`
		FileContent string `json:"file_content"`
		Version     string `json:"version"`
		SigningKey  string `json:"signing_key"`
		Algorithm   string `json:"algorithm"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if req.FilePath == "" {
		return BadRequest(c, "file_path is required")
	}
	if req.SigningKey == "" {
		return BadRequest(c, "signing_key is required")
	}
	content, err := base64.StdEncoding.DecodeString(req.FileContent)
	if err != nil || len(content) == 0 {
		return BadRequest(c, "file_content must be non-empty base64")
	}

	store := getFirmwareSigStore()
	path, err := firmwareFilePath(req.FilePath)
	if err != nil {
		return BadRequest(c, err.Error())
	}
	if err := os.MkdirAll(store.dir(), 0o755); err != nil {
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		logrus.WithError(err).Error("Save firmware file failed")
		return InternalError(c, "ERR_INTERNAL_ERROR")
	}

	signature, publicKey, verifyKey, err := signFirmwareContent(req.Algorithm, req.SigningKey, content)
	if err != nil {
		return BadRequest(c, err.Error())
	}

	rec := &firmwareSigRecord{
		ID:        fmt.Sprintf("fw-%d-%s", time.Now().UnixMilli(), shortHash(req.FilePath)),
		Filename:  filepath.Base(path),
		Version:   req.Version,
		Size:      int64(len(content)),
		Algorithm: algorithmNameNormalize(req.Algorithm),
		Signature: signature,
		PublicKey: publicKey,
		SignedAt:  time.Now(),
		Verified:  true,
		VerifyKey: verifyKey,
		FilePath:  path,
	}
	store.mu.Lock()
	store.records[rec.ID] = rec
	store.saveLocked()
	store.mu.Unlock()

	return Created(c, map[string]interface{}{
		"id":        rec.ID,
		"filename":  rec.Filename,
		"version":   rec.Version,
		"size":      rec.Size,
		"algorithm": rec.Algorithm,
		"signed":    true,
	})
}

func handleListFirmwareSignatures(c echo.Context) error {
	store := getFirmwareSigStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	list := make([]*firmwareSigRecord, 0, len(store.records))
	for _, r := range store.records {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].SignedAt.After(list[j].SignedAt) })
	items := make([]map[string]interface{}, 0, len(list))
	for _, r := range list {
		items = append(items, map[string]interface{}{
			"id":        r.ID,
			"filename":  r.Filename,
			"version":   r.Version,
			"size":      r.Size,
			"algorithm": r.Algorithm,
			"signature": r.Signature,
			"signed_at": r.SignedAt,
			"verified":  r.Verified,
		})
	}
	return OK(c, map[string]interface{}{"items": items, "total": len(items)})
}

func handleVerifyFirmwareNoID(c echo.Context) error {
	return BadRequest(c, "firmware signature id is required: POST /firmware/:id/verify")
}

func handleVerifyFirmware(c echo.Context) error {
	id := c.Param("id")
	store := getFirmwareSigStore()
	store.mu.Lock()
	rec := store.records[id]
	store.mu.Unlock()
	if rec == nil {
		return NotFound(c, "firmware signature not found")
	}
	content, err := os.ReadFile(rec.FilePath)
	if err != nil {
		store.mu.Lock()
		rec.Verified = false
		store.saveLocked()
		store.mu.Unlock()
		return OK(c, map[string]interface{}{"verified": false, "message": "firmware file missing"})
	}
	ok, err := verifyFirmwareContent(rec, content)
	if err != nil {
		return OK(c, map[string]interface{}{"verified": false, "message": err.Error()})
	}
	store.mu.Lock()
	rec.Verified = ok
	store.saveLocked()
	store.mu.Unlock()
	return OK(c, map[string]interface{}{"verified": ok, "message": map[bool]string{true: "Firmware signature verified", false: "Firmware signature mismatch"}[ok]})
}

// shortHash returns a short stable identifier fragment for a string.
func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:6]
}
