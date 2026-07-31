// Command mcpprobe is a throwaway diagnostic that calls the Kiro-hosted MCP
// endpoint (AmazonCodeWhispererStreamingService.InvokeMCP) using the same
// credentials kirocc already reads from the Kiro CLI database.
//
// Usage:
//
//	go run ./scripts/mcpprobe                       # tools/list
//	go run ./scripts/mcpprobe -call web_search -q "..."
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/config"
)

func main() {
	call := flag.String("call", "", "tool name to invoke via tools/call (empty = tools/list)")
	query := flag.String("q", "", "query argument for the tool call")
	flag.Parse()

	db, err := auth.OpenDB(config.DefaultDBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db:", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	creds, err := auth.ReadCredentials(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read credentials:", err)
		os.Exit(1)
	}
	fmt.Printf("region=%s authType=%s profileArn=%s tokenLen=%d\n",
		creds.Region, creds.AuthType, creds.ProfileARN, len(creds.AccessToken))

	body := map[string]any{
		"profileArn": creds.ProfileARN,
		"jsonrpc":    "2.0",
		"id":         "1",
	}
	if *call == "" {
		body["method"] = "tools/list"
		body["params"] = map[string]any{}
	} else {
		body["method"] = "tools/call"
		body["params"] = map[string]any{
			"name":      *call,
			"arguments": map[string]any{"query": *query},
		}
	}

	raw, _ := json.Marshal(body)
	endpoint := fmt.Sprintf("https://q.%s.amazonaws.com/", creds.Region)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		fmt.Fprintln(os.Stderr, "new request:", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonCodeWhispererStreamingService.InvokeMCP")
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("X-Amzn-Kiro-Profile-Arn", creds.ProfileARN)
	req.Header.Set("X-Amzn-Codewhisperer-Optout", "false")

	fmt.Printf("POST %s\nbody: %s\n\n", endpoint, raw)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "do:", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()

	out, _ := io.ReadAll(resp.Body)
	fmt.Println("status:", resp.Status)
	var pretty bytes.Buffer
	if json.Indent(&pretty, out, "", "  ") == nil {
		fmt.Println(pretty.String())
	} else {
		fmt.Println(string(out))
	}
}
