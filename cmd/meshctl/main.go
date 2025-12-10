package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "meshctl",
	Short: "Mesh control plane CLI",
	Long:  `meshctl is a command-line tool for managing the mesh load balancer control plane.`,
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(drainCmd)
	rootCmd.AddCommand(genkeysCmd)
	rootCmd.AddCommand(allowlistCmd)
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Query control plane status",
	Run: func(cmd *cobra.Command, args []string) {
		// Stub - would query meshd gRPC API for cluster status
		fmt.Println("Status: OK")
		fmt.Println("Registry entries: 0")
		fmt.Println("Gossip peers: 0")
	},
}

var drainCmd = &cobra.Command{
	Use:   "drain [node-id]",
	Short: "Drain a backend node",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		nodeID := args[0]
		// Stub - would send drain request to meshd
		fmt.Printf("Draining node: %s\n", nodeID)
		fmt.Println("Node marked for drain - traffic will stop routing to it")
	},
}

var genkeysCmd = &cobra.Command{
	Use:   "genkeys",
	Short: "Generate libp2p identity keypair",
	Run: func(cmd *cobra.Command, args []string) {
		// Stub - would generate Ed25519 keypair and print base64
		fmt.Println("Generated identity keypair:")
		fmt.Println("Public Key:  12D3KooWExample...")
		fmt.Println("Private Key: CAESQExample...")
		fmt.Println("Save the private key in your config file")
	},
}

var allowlistCmd = &cobra.Command{
	Use:   "allowlist",
	Short: "Manage node allowlist",
}

var allowlistAddCmd = &cobra.Command{
	Use:   "add [node-id]",
	Short: "Add a node to the allowlist",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		nodeID := args[0]
		fmt.Printf("Adding node to allowlist: %s\n", nodeID)
	},
}

var allowlistRemoveCmd = &cobra.Command{
	Use:   "remove [node-id]",
	Short: "Remove a node from the allowlist",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		nodeID := args[0]
		fmt.Printf("Removing node from allowlist: %s\n", nodeID)
	},
}

var allowlistListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all allowlisted nodes",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Allowlisted nodes:")
		fmt.Println("  - node-1")
		fmt.Println("  - node-2")
	},
}

func init() {
	allowlistCmd.AddCommand(allowlistAddCmd)
	allowlistCmd.AddCommand(allowlistRemoveCmd)
	allowlistCmd.AddCommand(allowlistListCmd)
}
