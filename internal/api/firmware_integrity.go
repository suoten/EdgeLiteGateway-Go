package api

// Real replacements for the /firmware/verify/* and /firmware/manifest/generate
// routes. The versions they replace answered {"valid":true} without reading a
// body, a key or a file, which for a firmware integrity endpoint is worse than
// a 404: an operator or CI job would ship an image nobody checked.
//
// The frontend (firmwareApi in api/index.ts) posts multipart/form-data with an
// uploaded artefact, so that is the primary contract; a JSON body naming a
// stored record id is also accepted for CI use. Nothing here writes the uploaded
// bytes anywhere - verification is read-only.

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/security"
)

// maxFirmwareUpload mirrors the 50 MB ceiling the frontend applies, so a client
// that skips its own check cannot stream an unbounded file into memory.
const maxFirmwareUpload = 50 << 20

// errFirmwareNoInput means the caller sent neither an upload nor a record id. For
// the manifest endpoint that is a legitimate request (manifest every stored
// record), for the verify endpoints it is a 400.
var errFirmwareNoInput = errors.New("an uploaded file or a stored record id is required")

// errFirmwareRecordNotFound names a record id that the store does not have, so
// the handlers can answer with the code that says which of the two went wrong.
var errFirmwareRecordNotFound = errors.New("not found")

// RegisterFirmwareIntegrityRoutes mounts the honest firmware integrity endpoints
// on the same paths the frontend calls, replacing the aliases that reported every
// artefact as verified.
func RegisterFirmwareIntegrityRoutes(g *echo.Group) {
	g.POST("/verify/signature", handleVerifyFirmwareSignatureReal, requirePermission(security.PermOTAManage))
	g.POST("/verify/hash", handleVerifyFirmwareHashReal, requirePermission(security.PermOTAManage))
	g.POST("/manifest/generate", handleGenerateFirmwareManifestReal, requirePermission(security.PermOTAManage))
}

// firmwareRequest carries both shapes of input: the multipart form the UI sends
// and the JSON body scripts send.
type firmwareRequest struct {
	ID           string `json:"id"`
	FileContent  string `json:"file_content"` // base64, JSON form
	ExpectedHash string `json:"expected_hash"`
	Signature    string `json:"signature"`
	PublicKey    string `json:"public_key"`
	Algorithm    string `json:"algorithm"`
	Version      string `json:"version"`
	Description  string `json:"description"`
}

// firmwareInput is what the verify endpoints work from: the bytes to check, where
// they came from, and the stored record when one was named (nil for a pure upload).
// It is returned even when the read failed, so a handler can still report the
// version and fields the caller did manage to send.
type firmwareInput struct {
	req     *firmwareRequest
	record  *firmwareSigRecord
	content []byte
	source  string
}

// firmwareErrorResponse maps a readFirmwareRequest failure onto an honest code.
func firmwareErrorResponse(c echo.Context, err error) error {
	if errors.Is(err, errFirmwareRecordNotFound) {
		return BadRequest(c, "ERR_FW_RECORD_NOT_FOUND: "+err.Error())
	}
	return BadRequest(c, "ERR_FW_CONTENT_UNREADABLE: "+err.Error())
}

// readFirmwareRequest binds either a multipart upload or a JSON body and returns
// the bytes to check together with where they came from.
func readFirmwareRequest(c echo.Context) (*firmwareInput, error) {
	req := &firmwareRequest{}
	in := &firmwareInput{req: req}
	ct := c.Request().Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := c.Request().ParseMultipartForm(maxFirmwareUpload); err != nil {
			return in, fmt.Errorf("malformed multipart form: %w", err)
		}
		req.ID = strings.TrimSpace(c.FormValue("id"))
		req.ExpectedHash = strings.TrimSpace(c.FormValue("expected_hash"))
		req.Signature = strings.TrimSpace(c.FormValue("signature"))
		req.PublicKey = strings.TrimSpace(c.FormValue("public_key"))
		req.Algorithm = strings.TrimSpace(c.FormValue("algorithm"))
		req.Version = strings.TrimSpace(c.FormValue("version"))
		req.Description = strings.TrimSpace(c.FormValue("description"))

		fileHeader, ferr := c.FormFile("file")
		if ferr != nil {
			// No upload: fall back to the artefact of the named stored record.
			rec, content, err := storedFirmwareBytes(req.ID)
			if err != nil {
				return in, err
			}
			in.record, in.content, in.source = rec, content, rec.Filename
			return in, nil
		}
		content, err := readMultipart(fileHeader)
		if err != nil {
			return in, err
		}
		in.content = content
		in.source = "uploaded file " + fileHeader.Filename
		if req.ID != "" {
			rec, err := firmwareStoredRecord(req.ID)
			if err != nil {
				return in, err
			}
			in.record = rec
		}
		return in, nil
	}

	if err := json.NewDecoder(io.LimitReader(c.Request().Body, maxFirmwareUpload)).Decode(req); err != nil && !errors.Is(err, io.EOF) {
		return in, errors.New("invalid JSON body")
	}
	if req.FileContent != "" {
		content, err := base64.StdEncoding.DecodeString(req.FileContent)
		if err != nil {
			return in, errors.New("file_content must be base64")
		}
		if len(content) == 0 {
			return in, errors.New("file_content must not be empty")
		}
		in.content = content
		in.source = "request body"
		if req.ID != "" {
			rec, err := firmwareStoredRecord(req.ID)
			if err != nil {
				return in, err
			}
			in.record = rec
		}
		return in, nil
	}
	rec, content, err := storedFirmwareBytes(req.ID)
	if err != nil {
		return in, err
	}
	in.record, in.content, in.source = rec, content, rec.Filename
	return in, nil
}

func readMultipart(fh *multipart.FileHeader) ([]byte, error) {
	if fh.Size > maxFirmwareUpload {
		return nil, fmt.Errorf("uploaded file is %d bytes, the limit is %d", fh.Size, maxFirmwareUpload)
	}
	f, err := fh.Open()
	if err != nil {
		return nil, fmt.Errorf("uploaded file cannot be opened: %w", err)
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, maxFirmwareUpload+1))
	if err != nil {
		return nil, fmt.Errorf("uploaded file cannot be read: %w", err)
	}
	if int64(len(content)) > maxFirmwareUpload {
		return nil, fmt.Errorf("uploaded file is larger than the %d byte limit", maxFirmwareUpload)
	}
	if len(content) == 0 {
		return nil, errors.New("uploaded file is empty")
	}
	return content, nil
}

// storedFirmwareBytes reads the artefact belonging to a stored signature record.
func storedFirmwareBytes(id string) (*firmwareSigRecord, []byte, error) {
	rec, err := firmwareStoredRecord(id)
	if err != nil {
		return nil, nil, err
	}
	content, err := os.ReadFile(rec.FilePath)
	if err != nil {
		return rec, nil, fmt.Errorf("stored firmware file %s cannot be read: %w", rec.Filename, err)
	}
	return rec, content, nil
}

func firmwareStoredRecord(id string) (*firmwareSigRecord, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errFirmwareNoInput
	}
	store := getFirmwareSigStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	rec := store.records[id]
	if rec == nil {
		return nil, fmt.Errorf("firmware signature %s not found: %w", id, errFirmwareRecordNotFound)
	}
	cp := *rec
	return &cp, nil
}

// firmwareDigest returns the hex digest of content under the named algorithm.
// An empty name means sha256, which is what the UI labels the field with.
func firmwareDigest(algorithm string, content []byte) (string, error) {
	var h hash.Hash
	switch strings.ToLower(strings.TrimSpace(algorithm)) {
	case "", "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	case "sha1":
		h = sha1.New()
	case "md5":
		h = md5.New()
	default:
		return "", fmt.Errorf("unsupported hash algorithm: %s", algorithm)
	}
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func normalizeExpectedHash(want string) string {
	w := strings.ToLower(strings.TrimSpace(want))
	for _, p := range []string{"sha256:", "sha512:", "sha1:", "md5:"} {
		w = strings.TrimPrefix(w, p)
	}
	return w
}

// handleVerifyFirmwareSignatureReal checks a signature over real bytes: either an
// uploaded artefact plus signature/key, or a stored record id.
func handleVerifyFirmwareSignatureReal(c echo.Context) error {
	in, err := readFirmwareRequest(c)
	if err != nil {
		return firmwareErrorResponse(c, err)
	}

	// Which signature gets checked: the one the caller sent, or the one the store
	// holds for that record. Explicit key material always wins so a CI job can
	// verify an image against a publisher key the gateway never saw.
	check := &firmwareSigRecord{
		Algorithm: algorithmNameNormalize(in.req.Algorithm),
		Signature: in.req.Signature,
		PublicKey: in.req.PublicKey,
		// HMAC keeps its key in the field the UI calls public_key; it is symmetric,
		// so there is no separate verification key.
		VerifyKey: in.req.PublicKey,
	}
	filename := in.source
	recordID := ""
	if check.Signature == "" {
		if in.record == nil {
			return BadRequest(c, "ERR_FW_SIGNATURE_MISSING: no signature to check; send signature+algorithm with the upload, or an id of a signed record")
		}
		check = &firmwareSigRecord{
			Algorithm: in.record.Algorithm,
			Signature: in.record.Signature,
			PublicKey: in.record.PublicKey,
			VerifyKey: in.record.VerifyKey,
		}
		filename = in.record.Filename
		recordID = in.record.ID
	}

	ok, verr := verifyFirmwareContent(check, in.content)
	message := "signature does not match these bytes"
	if verr != nil {
		message, ok = verr.Error(), false
	} else if ok {
		message = "signature verified against " + in.source
	}
	if recordID != "" {
		// The stored flag follows the last real check, so the list cannot keep
		// showing "verified" after the artefact was swapped out from under it.
		store := getFirmwareSigStore()
		store.mu.Lock()
		if stored := store.records[recordID]; stored != nil {
			stored.Verified = ok
			store.saveLocked()
		}
		store.mu.Unlock()
	}
	return OK(c, map[string]interface{}{
		"valid":     ok,
		"id":        recordID,
		"filename":  filename,
		"algorithm": check.Algorithm,
		"size":      len(in.content),
		"source":    in.source,
		"message":   message,
	})
}

// handleVerifyFirmwareHashReal computes the digest of the real bytes and compares
// it with expected_hash when one was sent.
func handleVerifyFirmwareHashReal(c echo.Context) error {
	in, err := readFirmwareRequest(c)
	if err != nil {
		return firmwareErrorResponse(c, err)
	}
	digest, err := firmwareDigest(in.req.Algorithm, in.content)
	if err != nil {
		return BadRequest(c, "ERR_FW_HASH_ALGORITHM: "+err.Error())
	}
	algorithm := strings.ToLower(strings.TrimSpace(in.req.Algorithm))
	if algorithm == "" {
		algorithm = "sha256"
	}
	data := map[string]interface{}{
		"hash":      digest,
		"algorithm": algorithm,
		"size":      len(in.content),
		"source":    in.source,
		"valid":     false,
		"message":   "expected_hash was not sent, so this is the computed digest only",
	}
	if in.req.ID != "" {
		data["record_id"] = in.req.ID
	}
	expected := normalizeExpectedHash(in.req.ExpectedHash)
	if expected != "" {
		match := digest == expected
		data["valid"] = match
		if match {
			data["message"] = "digest matches expected_hash"
		} else {
			data["message"] = "digest does not match expected_hash"
		}
	}
	return OK(c, data)
}

// handleGenerateFirmwareManifestReal builds a manifest. With an upload (or a
// record id) it describes that one artefact from the bytes it actually received;
// with neither it reports every stored record, re-hashing what is still on disk.
func handleGenerateFirmwareManifestReal(c echo.Context) error {
	in, err := readFirmwareRequest(c)
	if err != nil && !errors.Is(err, errFirmwareNoInput) {
		return firmwareErrorResponse(c, err)
	}
	if err != nil {
		// Nothing was uploaded and no record was named: the manifest then covers the
		// records this gateway actually signed, which may be none. The fields the
		// caller did send (version) still describe the manifest itself.
		in.record, in.content, in.source = nil, nil, ""
	}
	if len(in.content) > 0 {
		digest, derr := firmwareDigest(in.req.Algorithm, in.content)
		if derr != nil {
			return BadRequest(c, "ERR_FW_HASH_ALGORITHM: "+derr.Error())
		}
		algorithm := strings.ToLower(strings.TrimSpace(in.req.Algorithm))
		if algorithm == "" {
			algorithm = "sha256"
		}
		version := in.req.Version
		if version == "" && in.record != nil {
			version = in.record.Version
		}
		entry := map[string]interface{}{
			"id":          in.req.ID,
			"filename":    in.source,
			"version":     version,
			"description": in.req.Description,
			"size":        len(in.content),
			"algorithm":   algorithm,
			"sha256":      digest,
			"signature":   in.req.Signature,
			"signed_at":   time.Now().Format(time.RFC3339),
		}
		if in.record != nil {
			entry["signature"] = in.record.Signature
		}
		return OK(c, map[string]interface{}{
			"generated_at":     time.Now().Format(time.RFC3339),
			"manifest_version": version,
			"entries":          []interface{}{entry},
			"total":            1,
			"missing_files":    0,
			"scope":            "uploaded artefact",
		})
	}

	store := getFirmwareSigStore()
	store.mu.Lock()
	records := make([]*firmwareSigRecord, 0, len(store.records))
	for _, r := range store.records {
		cp := *r
		records = append(records, &cp)
	}
	store.mu.Unlock()
	if len(records) == 0 {
		return OK(c, map[string]interface{}{
			"generated_at":     time.Now().Format(time.RFC3339),
			"manifest_version": in.req.Version,
			"entries":          []interface{}{},
			"total":            0,
			"missing_files":    0,
			"scope":            "stored records",
			"note":             "no firmware has been signed by this gateway yet, so there is nothing to manifest",
		})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].SignedAt.After(records[j].SignedAt) })

	entries := make([]interface{}, 0, len(records))
	missing := 0
	for _, r := range records {
		entry := map[string]interface{}{
			"id":        r.ID,
			"filename":  r.Filename,
			"version":   r.Version,
			"size":      r.Size,
			"algorithm": r.Algorithm,
			"signature": r.Signature,
			"signed_at": r.SignedAt.Format(time.RFC3339),
			"sha256":    "",
			"on_disk":   false,
			"verified":  false,
		}
		fileBytes, rerr := os.ReadFile(r.FilePath)
		if rerr != nil {
			missing++
			entry["note"] = "stored file is no longer on disk"
			entries = append(entries, entry)
			continue
		}
		sum := sha256.Sum256(fileBytes)
		entry["sha256"] = hex.EncodeToString(sum[:])
		entry["size"] = int64(len(fileBytes))
		entry["on_disk"] = true
		if ok, verr := verifyFirmwareContent(r, fileBytes); verr == nil {
			entry["verified"] = ok
		}
		entries = append(entries, entry)
	}
	return OK(c, map[string]interface{}{
		"generated_at":     time.Now().Format(time.RFC3339),
		"manifest_version": in.req.Version,
		"entries":          entries,
		"total":            len(entries),
		"missing_files":    missing,
		"scope":            "stored records",
	})
}
