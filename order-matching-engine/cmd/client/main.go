// Package main provides a CLI client for the order matching engine.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	// Flags
	serverURL := flag.String("server", "http://localhost:8080", "Server URL")

	// Subcommands
	submitCmd := flag.NewFlagSet("submit", flag.ExitOnError)
	submitSymbol := submitCmd.String("symbol", "AAPL", "Stock symbol")
	submitSide := submitCmd.String("side", "buy", "Order side (buy/sell)")
	submitType := submitCmd.String("type", "limit", "Order type (market/limit/ioc/fok)")
	submitPrice := submitCmd.String("price", "150.00", "Order price")
	submitQty := submitCmd.Int64("qty", 100, "Order quantity")
	submitAccount := submitCmd.String("account", "TRADER1", "Account ID")

	cancelCmd := flag.NewFlagSet("cancel", flag.ExitOnError)
	cancelSymbol := cancelCmd.String("symbol", "", "Stock symbol")
	cancelOrderID := cancelCmd.Uint64("order-id", 0, "Order ID to cancel")

	bookCmd := flag.NewFlagSet("book", flag.ExitOnError)
	bookSymbol := bookCmd.String("symbol", "AAPL", "Stock symbol")
	bookLevels := bookCmd.Int("levels", 5, "Number of levels to show")

	accountCmd := flag.NewFlagSet("account", flag.ExitOnError)
	accountID := accountCmd.String("id", "TRADER1", "Account ID")

	statsCmd := flag.NewFlagSet("stats", flag.ExitOnError)

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	// Parse server flag first
	flag.Parse()

	switch os.Args[1] {
	case "submit":
		submitCmd.Parse(os.Args[2:])
		submitOrder(*serverURL, *submitSymbol, *submitSide, *submitType, *submitPrice, *submitQty, *submitAccount)

	case "cancel":
		cancelCmd.Parse(os.Args[2:])
		cancelOrder(*serverURL, *cancelSymbol, *cancelOrderID)

	case "book":
		bookCmd.Parse(os.Args[2:])
		getBook(*serverURL, *bookSymbol, *bookLevels)

	case "account":
		accountCmd.Parse(os.Args[2:])
		getAccount(*serverURL, *accountID)

	case "stats":
		statsCmd.Parse(os.Args[2:])
		getStats(*serverURL)

	case "demo":
		runDemo(*serverURL)

	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Order Matching Engine Client

Usage:
  client <command> [options]

Commands:
  submit    Submit a new order
  cancel    Cancel an existing order
  book      View order book
  account   View account details
  stats     View system statistics
  demo      Run a demonstration

Examples:
  client submit -symbol AAPL -side buy -type limit -price 150.00 -qty 100 -account TRADER1
  client cancel -symbol AAPL -order-id 123
  client book -symbol AAPL -levels 10
  client account -id TRADER1
  client stats
  client demo`)
}

func submitOrder(serverURL, symbol, side, orderType, price string, qty int64, account string) {
	req := map[string]interface{}{
		"symbol":     symbol,
		"side":       side,
		"type":       orderType,
		"price":      price,
		"quantity":   qty,
		"account_id": account,
	}

	resp, err := postJSON(serverURL+"/order", req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	fmt.Printf("Order Response:\n")
	printJSON(resp)
}

func cancelOrder(serverURL, symbol string, orderID uint64) {
	url := fmt.Sprintf("%s/cancel?symbol=%s&order_id=%d", serverURL, symbol, orderID)

	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("Cancel Response:\n")
	printJSONBytes(body)
}

func getBook(serverURL, symbol string, levels int) {
	url := fmt.Sprintf("%s/book?symbol=%s&levels=%d", serverURL, symbol, levels)

	resp, err := http.Get(url)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var data map[string]interface{}
	json.Unmarshal(body, &data)

	fmt.Printf("\n=== %s Order Book ===\n\n", symbol)

	// Print asks in reverse (so lowest is at bottom)
	if asks, ok := data["asks"].([]interface{}); ok {
		fmt.Println("ASKS:")
		for i := len(asks) - 1; i >= 0; i-- {
			if ask, ok := asks[i].(map[string]interface{}); ok {
				fmt.Printf("  %s: %.0f shares (%v orders)\n",
					ask["price"], ask["quantity"], ask["orders"])
			}
		}
	}

	fmt.Printf("--- Spread: %v ---\n", data["spread"])

	// Print bids
	if bids, ok := data["bids"].([]interface{}); ok {
		fmt.Println("BIDS:")
		for _, bid := range bids {
			if b, ok := bid.(map[string]interface{}); ok {
				fmt.Printf("  %s: %.0f shares (%v orders)\n",
					b["price"], b["quantity"], b["orders"])
			}
		}
	}

	fmt.Printf("\nMid Price: %v\n", data["mid"])
}

func getAccount(serverURL, accountID string) {
	url := fmt.Sprintf("%s/account?id=%s", serverURL, accountID)

	resp, err := http.Get(url)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("Account Details:\n")
	printJSONBytes(body)
}

func getStats(serverURL string) {
	resp, err := http.Get(serverURL + "/stats")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("System Statistics:\n")
	printJSONBytes(body)
}

func runDemo(serverURL string) {
	fmt.Println("=== Order Matching Engine Demo ===")

	// Step 1: Show initial book
	fmt.Println("1. Initial order book (empty):")
	getBook(serverURL, "AAPL", 5)

	// Step 2: Market maker posts liquidity
	fmt.Println("\n2. Market maker (MM1) posts buy orders:")
	submitOrder(serverURL, "AAPL", "buy", "limit", "149.00", 100, "MM1")
	submitOrder(serverURL, "AAPL", "buy", "limit", "148.50", 200, "MM1")
	submitOrder(serverURL, "AAPL", "buy", "limit", "148.00", 300, "MM1")

	fmt.Println("\n3. Market maker (MM1) posts sell orders:")
	submitOrder(serverURL, "AAPL", "sell", "limit", "151.00", 100, "MM1")
	submitOrder(serverURL, "AAPL", "sell", "limit", "151.50", 200, "MM1")
	submitOrder(serverURL, "AAPL", "sell", "limit", "152.00", 300, "MM1")

	// Step 3: Show book with liquidity
	fmt.Println("\n4. Order book with liquidity:")
	getBook(serverURL, "AAPL", 5)

	// Step 4: Trader executes against the book
	fmt.Println("\n5. Trader (TRADER1) buys 150 shares with market order:")
	submitOrder(serverURL, "AAPL", "buy", "market", "0", 150, "TRADER1")

	// Step 5: Show updated book
	fmt.Println("\n6. Order book after trade:")
	getBook(serverURL, "AAPL", 5)

	// Step 6: Show stats
	fmt.Println("\n7. System statistics:")
	getStats(serverURL)

	fmt.Println("\n=== Demo Complete ===")
}

func postJSON(url string, data interface{}) (map[string]interface{}, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	err = json.Unmarshal(body, &result)
	return result, err
}

func printJSON(data interface{}) {
	jsonBytes, _ := json.MarshalIndent(data, "", "  ")
	fmt.Println(string(jsonBytes))
}

func printJSONBytes(data []byte) {
	var obj interface{}
	json.Unmarshal(data, &obj)
	printJSON(obj)
}
