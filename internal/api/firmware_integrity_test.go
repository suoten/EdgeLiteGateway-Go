package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// The aliases these tests cover used to answer {"valid":true} for every request,
// including requests with no body. They now verify against the same store the
// signing endpoint writes, so a swapped-out artefact has to read as invalid.

func useFirmwareStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := fwSigStore
	fwSigOnce.Do(func() {}) // consume so getFirmwareSigStore keeps our instance
	fwSigStore = &firmwareSigStore{records: map[string]*firmwareSigRecord{}, path: filepath.Join(dir, "signatures.json")}
	t.Cleanup(func() { fwSigStore = prev })
	return dir
}

func callFirmware(t *testing.T, h func(echo.Context) error, body string) (int, map[string]interface{}, string) {
	t.Helper()
	c, rec := setupEcho(http.MethodPost, "/api/v1/firmware/verify", body)
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		Code      int                    `json:"code"`
		ErrorCode string                 `json:"error_code"`
		Data      map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body, err)
	}
	return rec.Code, env.Data, env.ErrorCode
}

func signTestFirmware(t *testing.T, name, content, key string) string {
	t.Helper()
	body := `{"file_path":"` + name + `","file_content":"` + base64.StdEncoding.EncodeToString([]byte(content)) +
		`","signing_key":"` + key + `","algorithm":"hmac256","version":"1.2.3"}`
	c, rec := setupEcho(http.MethodPost, "/api/v1/firmware/sign", body)
	if err := handleSignFirmware(c); err != nil {
		t.Fatalf("handleSignFirmware: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("sign returned %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable sign envelope: %v", err)
	}
	if !strings.HasPrefix(env.Data.ID, "fw-") {
		t.Fatalf("sign returned no record id: %s", rec.Body)
	}
	return env.Data.ID
}

func TestVerifyFirmwareSignatureChecksRealContent(t *testing.T) {
	dir := useFirmwareStore(t)
	id := signTestFirmware(t, "app.bin", "firmware-bytes-A", "key-1")

	code, data, errCode := callFirmware(t, handleVerifyFirmwareSignatureReal, `{"id":"`+id+`"}`)
	if code != http.StatusOK || errCode != "" {
		t.Fatalf("verify returned %d/%s, want 200 with no error code", code, errCode)
	}
	if data["valid"] != true {
		t.Fatalf("untouched artefact reported valid=%v (%v)", data["valid"], data["message"])
	}
	if data["algorithm"] != "hmac256" {
		t.Fatalf("verify did not report the recorded algorithm: %#v", data)
	}

	// Swap the stored file: the same id must now read as invalid.
	if err := os.WriteFile(filepath.Join(dir, "app.bin"), []byte("firmware-bytes-B"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_, data, _ = callFirmware(t, handleVerifyFirmwareSignatureReal, `{"id":"`+id+`"}`)
	if data["valid"] != false {
		t.Fatalf("tampered artefact still reported valid: %#v", data)
	}
	store := getFirmwareSigStore()
	store.mu.Lock()
	stored := store.records[id].Verified
	store.mu.Unlock()
	if stored {
		t.Fatalf("the stored verified flag was not cleared after a failed check")
	}

	// A body wins over the stored file, which is how a CI job checks an image it has
	// not uploaded.
	_, data, _ = callFirmware(t, handleVerifyFirmwareSignatureReal,
		`{"id":"`+id+`","file_content":"`+base64.StdEncoding.EncodeToString([]byte("firmware-bytes-A"))+`"}`)
	if data["valid"] != true || data["source"] != "request body" {
		t.Fatalf("inline content not honoured: %#v", data)
	}
}

func TestVerifyFirmwareSignatureRejectsUnknownID(t *testing.T) {
	useFirmwareStore(t)
	code, _, errCode := callFirmware(t, handleVerifyFirmwareSignatureReal, `{"id":"fw-nope"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown id returned %d, want 400", code)
	}
	if !strings.HasPrefix(errCode, "ERR_FW_RECORD_NOT_FOUND") {
		t.Fatalf("unknown id error code = %q, want ERR_FW_RECORD_NOT_FOUND with the reason", errCode)
	}
	if !strings.Contains(errCode, "fw-nope") {
		t.Fatalf("error code lost which id was missing: %q", errCode)
	}
	// A body with nothing to check must not be reported as a successful check.
	code, _, errCode = callFirmware(t, handleVerifyFirmwareSignatureReal, `{}`)
	if code != http.StatusBadRequest || !strings.HasPrefix(errCode, "ERR_FW_CONTENT_UNREADABLE") {
		t.Fatalf("missing input returned %d/%q, want 400 ERR_FW_CONTENT_UNREADABLE", code, errCode)
	}
}

func TestVerifyFirmwareHashReportsWhatItChecked(t *testing.T) {
	useFirmwareStore(t)
	id := signTestFirmware(t, "img.bin", "payload-1", "key-1")
	sum := sha256.Sum256([]byte("payload-1"))
	got := hex.EncodeToString(sum[:])

	_, data, _ := callFirmware(t, handleVerifyFirmwareHashReal, `{"id":"`+id+`","expected_hash":"`+got+`"}`)
	if data["valid"] != true || data["hash"] != got {
		t.Fatalf("matching digest reported invalid: %#v", data)
	}

	_, data, _ = callFirmware(t, handleVerifyFirmwareHashReal, `{"id":"`+id+`","expected_hash":"SHA256:`+strings.Repeat("0", 64)+`"}`)
	if data["valid"] != false {
		t.Fatalf("mismatched digest reported valid: %#v", data)
	}

	// With nothing to compare against the answer is "computed", not "verified".
	code, data, _ := callFirmware(t, handleVerifyFirmwareHashReal, `{"id":"`+id+`"}`)
	if code != http.StatusOK {
		t.Fatalf("bare hash request returned %d, want 200", code)
	}
	if data["valid"] != false || data["hash"] != got {
		t.Fatalf("bare hash request claimed validity: %#v", data)
	}
	if !strings.Contains(data["message"].(string), "expected_hash") {
		t.Fatalf("bare hash request did not explain why valid is false: %#v", data["message"])
	}
}

func TestGenerateFirmwareManifestReflectsDisk(t *testing.T) {
	dir := useFirmwareStore(t)
	id := signTestFirmware(t, "m.bin", "manifest-payload", "key-1")
	sum := sha256.Sum256([]byte("manifest-payload"))

	code, data, _ := callFirmware(t, handleGenerateFirmwareManifestReal, `{"version":"rel-7"}`)
	if code != http.StatusOK {
		t.Fatalf("manifest returned %d, want 200", code)
	}
	if data["manifest_version"] != "rel-7" || data["missing_files"] != float64(0) {
		t.Fatalf("manifest header wrong: %#v", data)
	}
	entries, ok := data["entries"].([]interface{})
	if !ok || len(entries) != 1 {
		t.Fatalf("manifest entries = %#v, want one", data["entries"])
	}
	entry := entries[0].(map[string]interface{})
	if entry["id"] != id || entry["on_disk"] != true || entry["verified"] != true {
		t.Fatalf("entry does not describe the signed record: %#v", entry)
	}
	if entry["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("entry sha256 = %v, want the digest of the stored bytes", entry["sha256"])
	}

	if err := os.Remove(filepath.Join(dir, "m.bin")); err != nil {
		t.Fatalf("remove artefact: %v", err)
	}
	_, data, _ = callFirmware(t, handleGenerateFirmwareManifestReal, `{}`)
	if data["missing_files"] != float64(1) {
		t.Fatalf("missing file not counted: %#v", data)
	}
	entry = data["entries"].([]interface{})[0].(map[string]interface{})
	if entry["on_disk"] != false || entry["verified"] != false || entry["sha256"] != "" {
		t.Fatalf("deleted artefact still looks healthy: %#v", entry)
	}
}

// firmwareSignatureOf reads the signature the store actually holds for a record,
// so the test uploads a real one instead of a hand-made hex string.
func firmwareSignatureOf(t *testing.T, id string) string {
	t.Helper()
	store := getFirmwareSigStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	rec := store.records[id]
	if rec == nil {
		t.Fatalf("record %s is not in the store", id)
	}
	return rec.Signature
}

// callFirmwareMultipart drives the contract the UI actually uses: firmwareApi in
// api/index.ts posts multipart/form-data with a File, so a JSON-only test would
// leave the shipped request shape unproven.
func callFirmwareMultipart(t *testing.T, h func(echo.Context) error, fields map[string]string, fileName string, fileContent []byte) (int, map[string]interface{}, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("WriteField(%s): %v", k, err)
		}
	}
	if fileName != "" {
		part, err := mw.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := part.Write(fileContent); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/firmware/verify/signature", &buf)
	req.Header.Set(echo.HeaderContentType, mw.FormDataContentType())
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		ErrorCode string                 `json:"error_code"`
		Data      map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body, err)
	}
	return rec.Code, env.Data, env.ErrorCode
}

func TestFirmwareIntegrityAcceptsMultipartUpload(t *testing.T) {
	useFirmwareStore(t)
	const payload = "firmware-bytes-A"
	sum := sha256.Sum256([]byte(payload))
	got := hex.EncodeToString(sum[:])

	// The UI sends signature + key + algorithm next to the file; a matching
	// artefact has to read as valid and say which bytes it checked.
	code, data, errCode := callFirmwareMultipart(t, handleVerifyFirmwareSignatureReal,
		map[string]string{"signature": "", "public_key": "key-1", "algorithm": "hmac256"}, "ui.bin", []byte(payload))
	// No signature was sent, so there is genuinely nothing to check.
	if code != http.StatusBadRequest || !strings.HasPrefix(errCode, "ERR_FW_SIGNATURE_MISSING") {
		t.Fatalf("upload without a signature returned %d/%q, want 400 ERR_FW_SIGNATURE_MISSING", code, errCode)
	}

	id := signTestFirmware(t, "ui.bin", payload, "key-1")
	sig := firmwareSignatureOf(t, id)
	code, data, errCode = callFirmwareMultipart(t, handleVerifyFirmwareSignatureReal,
		map[string]string{"signature": sig, "public_key": "key-1", "algorithm": "hmac256"}, "renamed.bin", []byte(payload))
	if code != http.StatusOK || errCode != "" {
		t.Fatalf("signed upload returned %d/%s, want 200", code, errCode)
	}
	if data["valid"] != true {
		t.Fatalf("signed upload reported valid=%v (%v)", data["valid"], data["message"])
	}
	if data["source"] != "uploaded file renamed.bin" {
		t.Fatalf("upload did not report the bytes it checked: %#v", data)
	}

	_, data, _ = callFirmwareMultipart(t, handleVerifyFirmwareSignatureReal,
		map[string]string{"signature": sig, "public_key": "key-1", "algorithm": "hmac256"}, "renamed.bin", []byte("firmware-bytes-tampered"))
	if data["valid"] != false {
		t.Fatalf("tampered upload reported valid: %#v", data)
	}

	// Hash check over the same shape.
	_, data, _ = callFirmwareMultipart(t, handleVerifyFirmwareHashReal,
		map[string]string{"expected_hash": got}, "ui.bin", []byte(payload))
	if data["valid"] != true || data["hash"] != got || data["size"] != float64(len(payload)) {
		t.Fatalf("multipart hash check wrong: %#v", data)
	}

	// A manifest built from an upload describes that upload, not the whole store.
	_, data, _ = callFirmwareMultipart(t, handleGenerateFirmwareManifestReal,
		map[string]string{"version": "ui-rel-1"}, "ui.bin", []byte(payload))
	if data["scope"] != "uploaded artefact" || data["manifest_version"] != "ui-rel-1" || data["total"] != float64(1) {
		t.Fatalf("multipart manifest wrong: %#v", data)
	}
	entry := data["entries"].([]interface{})[0].(map[string]interface{})
	if entry["sha256"] != got || entry["filename"] != "uploaded file ui.bin" {
		t.Fatalf("multipart manifest entry wrong: %#v", entry)
	}
}
