package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/lunc/mesh/pkg/pki"
	"go.uber.org/zap"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8443"
	}

	// Create mesh CA
	ca, err := pki.NewCA("MeshCA", 10*365*24*time.Hour) // 10 years
	if err != nil {
		log.Fatalf("Failed to create CA: %v", err)
	}

	logger.Info("Mesh PKI server starting",
		zap.String("ca_subject", ca.Cert.Subject.CommonName),
		zap.Time("valid_until", ca.Cert.NotAfter),
	)

	// HTTP API for certificate signing requests
	http.HandleFunc("/sign", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		nodeID := r.URL.Query().Get("node_id")
		if nodeID == "" {
			http.Error(w, "node_id required", http.StatusBadRequest)
			return
		}

		// Issue certificate for node
		cert, key, err := ca.SignNodeCert(nodeID, 365*24*time.Hour) // 1 year
		if err != nil {
			logger.Error("Failed to sign certificate", zap.String("node_id", nodeID), zap.Error(err))
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"cert":"%s","key":"%s"}`, cert, key)
		logger.Info("Issued certificate", zap.String("node_id", nodeID))
	})

	http.HandleFunc("/ca", func(w http.ResponseWriter, r *http.Request) {
		// Return CA certificate for verification
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(ca.Cert.Raw)
	})

	addr := fmt.Sprintf(":%s", port)
	logger.Info("Starting HTTP API", zap.String("addr", addr))
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
