package main

import (
	"bufio"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ocsp"
)

func check(err error, w *http.ResponseWriter) {
	if err != nil {
		http.Error(*w, err.Error(), http.StatusInternalServerError)
		log.Fatal(err)
	}
}

// Handle an OCSP request using OpenSSL CLI.
// Nonce extension [1] is used, and the responder's cert is included in the
// response. [2]
//
// [1]: https://datatracker.ietf.org/doc/html/rfc6960#section-4.4.1
// [2]: https://datatracker.ietf.org/doc/html/rfc6960#section-4.2.1
func opensslHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Only POST requests are supported", http.StatusMethodNotAllowed)
		return
	}

	body, err := ioutil.ReadAll(r.Body)
	check(err, &w)

	// TODO: Later replace wd with an empty string to use OS default
	// directory for temporary files, and delete the working directory when
	// done.
	workDir, err := ioutil.TempDir("wd", "")
	check(err, &w)

	reqFileName := filepath.Join(workDir, "req.der")
	respFileName := filepath.Join(workDir, "resp.der")

	// Write certificate request
	reqFile, err := os.Create(reqFileName)
	check(err, &w)
	_, err = reqFile.Write(body)
	check(err, &w)
	reqFile.Close()

	// Create certificate response
	err = exec.Command("openssl", "ocsp",
		"-index", "data/index.txt",
		"-CAfile", "data/get.eduroam.se.pem",
		"-rsigner", "data/get.eduroam.se.pem",
		"-rkey", "data/get.eduroam.se.key.pem",
		"-CA", "data/get.eduroam.se.pem",
		"-reqin", reqFileName,
		"-respout", respFileName).Run()
	check(err, &w)

	// Read certificate response
	resp, err := os.ReadFile(respFileName)
	check(err, &w)

	w.Header().Set("Content-Type", "application/ocsp-response")
	w.Header().Set("Content-Length", strconv.Itoa(len(resp)))
	w.Write(resp)
}

func register(m map[int64]bool, serial int64, status string) (map[int64]bool, error) {
	switch status {
	case "V":
		m[serial] = true
	case "R":
		m[serial] = false
	default:
		return m, fmt.Errorf("Unrecognized status field in index file: %s", status)
	}
	return m, nil
}

func readPEM(filename string, w *http.ResponseWriter) *pem.Block {
	pemData, err := os.ReadFile(filename)
	check(err, w)
	block, _ := pem.Decode(pemData)
	if block == nil {
		check(fmt.Errorf("PEM parsing failure: %s", filename), w)
	}
	return block
}

// Handle an OCSP request using standard library and golang.org/x/crypto/ocsp.
// Nonce extension [1] is NOT used, and the responder's cert is NOT included in
// the response. [2]
//
// [1]: https://datatracker.ietf.org/doc/html/rfc6960#section-4.4.1
// [2]: https://datatracker.ietf.org/doc/html/rfc6960#section-4.2.1
func goHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Only POST requests are supported", http.StatusMethodNotAllowed)
		return
	}

	// Read index.txt
	//
	// TODO: Index file is for a single CA. If we decide to use go crypto we
	// will probably replace the OpenSSL index format entirely.
	// TODO: Optimization: Only when needed.
	index := make(map[int64]bool) // TODO: Do we need big.Int?
	file, err := os.Open("data/index.txt")
	check(err, &w)
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var status string
		var serial int64
		parts := strings.Split(scanner.Text(), "\t")
		status = parts[0]
		serial, err = strconv.ParseInt(parts[3], 10, 64)
		check(err, &w)
		_, err = register(index, serial, status)
		check(err, &w)
	}

	// Read CA certificate, key
	// TODO: Optimization: Only when needed.
	pemBlock := readPEM("data/get.eduroam.se.pem", &w)
	cert, err := x509.ParseCertificate(pemBlock.Bytes)
	check(err, &w)
	pemBlock = readPEM("data/get.eduroam.se.key.pem", &w)
	key, err := x509.ParseECPrivateKey(pemBlock.Bytes)
	check(err, &w)

	// Parse request
	body, err := ioutil.ReadAll(r.Body)
	check(err, &w)
	req, err := ocsp.ParseRequest(body)

	// Create response
	now := time.Now()
	template := ocsp.Response{
		SerialNumber: req.SerialNumber,
		IssuerHash:   crypto.SHA1,
		ThisUpdate:   now,
		// NextUpdate:   now.Add(time.Hour), // TODO
	}

	serial := req.SerialNumber.Int64()
	if !req.SerialNumber.IsInt64() {
		check(errors.New("Requested serial number is larger than 64 bits"), &w)
	}

	if s, found := index[serial]; !found {
		template.Status = ocsp.Unknown
	} else if s {
		template.Status = ocsp.Good
	} else {
		template.Status = ocsp.Revoked
		template.RevokedAt = now
		template.RevocationReason = ocsp.Unspecified // TODO
	}

	// Sign response using CA certificate
	resp, err := ocsp.CreateResponse(cert, cert, template, key)
	check(err, &w)

	// Write HTTP response
	w.Header().Set("Content-Type", "application/ocsp-response")
	w.Header().Set("Content-Length", strconv.Itoa(len(resp)))
	w.Write(resp)
}

func main() {
	http.HandleFunc("/openssl", opensslHandler)
	http.HandleFunc("/go", goHandler)
	log.Fatal(http.ListenAndServe("localhost:8889", nil))
}
