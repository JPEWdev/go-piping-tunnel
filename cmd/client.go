package cmd

import (
	"fmt"
	"github.com/hashicorp/yamux"
	"github.com/nwtgck/go-piping-tunnel/io_progress"
	piping_tunnel_util "github.com/nwtgck/go-piping-tunnel/piping-tunnel-util"
	"github.com/nwtgck/go-piping-tunnel/util"
	"github.com/spf13/cobra"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
)

var clientHostPort int
var clientServerToClientBufSize uint
var clientYamux bool
var clientYamuxBufSize uint
var clientSymmetricallyEncrypts bool
var clientSymmetricallyEncryptPassphrase string
var clientCipherType string

func init() {
	RootCmd.AddCommand(clientCmd)
	clientCmd.Flags().IntVarP(&clientHostPort, "port", "p", 0, "TCP port of client host")
	clientCmd.Flags().UintVarP(&clientServerToClientBufSize, "s-to-c-buf-size", "", 16, "Buffer size of server-to-client in bytes")
	clientCmd.Flags().BoolVarP(&clientYamux, yamuxFlagLongName, "", false, "Multiplex connection by hashicorp/yamux")
	clientCmd.Flags().UintVarP(&clientYamuxBufSize, "yamux-buf-size", "", 4096, "yamux buffer size in bytes")
	clientCmd.Flags().BoolVarP(&clientSymmetricallyEncrypts, symmetricallyEncryptsFlagLongName, symmetricallyEncryptsFlagShortName, false, "Encrypt symmetrically")
	clientCmd.Flags().StringVarP(&clientSymmetricallyEncryptPassphrase, symmetricallyEncryptPassphraseFlagLongName, "", "", "Passphrase for encryption")
	clientCmd.Flags().StringVarP(&clientCipherType, cipherTypeFlagLongName, "", defaultCipherType, fmt.Sprintf("Cipher type: %s, %s", cipherTypeAesCtr, cipherTypeOpenpgp))
}

var clientCmd = &cobra.Command{
	Use:   "client",
	Short: "Run client-host",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Validate cipher-type
		if clientSymmetricallyEncrypts {
			if err := validateClientCipher(clientCipherType); err != nil {
				return nil
			}
		}
		clientToServerPath, serverToClientPath, err := generatePaths(args)
		if err != nil {
			return err
		}
		headers, err := piping_tunnel_util.ParseKeyValueStrings(headerKeyValueStrs)
		if err != nil {
			return err
		}
		httpClient := util.CreateHttpClient(insecure, httpWriteBufSize, httpReadBufSize)
		if dnsServer != "" {
			// Set DNS resolver
			httpClient.Transport.(*http.Transport).DialContext = util.CreateDialContext(dnsServer)
		}
		clientToServerUrl, err := util.UrlJoin(serverUrl, clientToServerPath)
		if err != nil {
			return err
		}
		serverToClientUrl, err := util.UrlJoin(serverUrl, serverToClientPath)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", clientHostPort))
		if err != nil {
			return err
		}
		// Print hint
		printHintForServerHost(ln, clientToServerUrl, serverToClientUrl, clientToServerPath, serverToClientPath)
		// Make user input passphrase if it is empty
		if clientSymmetricallyEncrypts {
			err = makeUserInputPassphraseIfEmpty(&clientSymmetricallyEncryptPassphrase)
			if err != nil {
				return err
			}
		}
		// Use multiplexer with yamux
		if clientYamux {
			fmt.Println("[INFO] Multiplexing with hashicorp/yamux")
			return clientHandleWithYamux(ln, httpClient, headers, clientToServerUrl, serverToClientUrl)
		}
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		fmt.Println("[INFO] accepted")
		// Refuse another new connection
		ln.Close()
		// If encryption is enabled
		if clientSymmetricallyEncrypts {
			duplex, err := makeDuplexWithEncryptionAndProgressIfNeed(httpClient, headers, clientToServerUrl, serverToClientUrl, clientSymmetricallyEncrypts, clientSymmetricallyEncryptPassphrase, clientCipherType)
			if err != nil {
				return err
			}
			fin := make(chan error)
			go func() {
				// TODO: hard code
				var buf = make([]byte, 16)
				_, err := io.CopyBuffer(duplex, conn, buf)
				fin <- err
			}()
			go func() {
				// TODO: hard code
				var buf = make([]byte, 16)
				_, err := io.CopyBuffer(conn, duplex, buf)
				fin <- err
			}()
			return util.CombineErrors(<-fin, <-fin)
		}
		var progress *io_progress.IOProgress = nil
		if showProgress {
			progress = io_progress.NewIOProgress(conn, conn, os.Stderr, makeProgressMessage)
		}
		var reader io.Reader = conn
		if progress != nil {
			reader = progress
		}
		req, err := http.NewRequest("POST", clientToServerUrl, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		for _, kv := range headers {
			req.Header.Set(kv.Key, kv.Value)
		}
		_, err = httpClient.Do(req)
		if err != nil {
			return err
		}
		req, err = http.NewRequest("GET", serverToClientUrl, nil)
		if err != nil {
			return err
		}
		for _, kv := range headers {
			req.Header.Set(kv.Key, kv.Value)
		}
		res, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		var writer io.Writer = conn
		if progress != nil {
			writer = progress
		}
		var buf = make([]byte, clientServerToClientBufSize)
		_, err = io.CopyBuffer(writer, res.Body, buf)
		fmt.Println()
		if err != nil {
			return err
		}
		fmt.Println("[INFO] Finished")

		return nil
	},
}

func printHintForServerHost(ln net.Listener, clientToServerUrl string, serverToClientUrl string, clientToServerPath string, serverToClientPath string) {
	// (from: https://stackoverflow.com/a/43425461)
	clientHostPort = ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[INFO] Client host listening on %d ...\n", clientHostPort)
	if !clientYamux {
		fmt.Println("[INFO] Hint: Server host (socat + curl)")
		fmt.Printf(
			"  socat 'EXEC:curl -NsS %s!!EXEC:curl -NsST - %s' TCP:127.0.0.1:<YOUR PORT>\n",
			strings.Replace(clientToServerUrl, ":", "\\:", -1),
			strings.Replace(serverToClientUrl, ":", "\\:", -1),
		)
	}
	fmt.Println("[INFO] Hint: Server host (piping-tunnel)")
	flags := ""
	if clientSymmetricallyEncrypts {
		flags += fmt.Sprintf("-%s ", symmetricallyEncryptsFlagShortName)
		if clientCipherType != defaultCipherType {
			flags += fmt.Sprintf("--%s=%s ", cipherTypeFlagLongName, clientCipherType)
		}
	}
	if clientYamux {
		flags += fmt.Sprintf("--%s ", yamuxFlagLongName)
	}
	fmt.Printf(
		"  piping-tunnel -s %s server -p <YOUR PORT> %s%s %s\n",
		serverUrl,
		flags,
		clientToServerPath,
		serverToClientPath,
	)
	fmt.Println("    OR")
	fmt.Printf(
		"  piping-tunnel -s %s socks %s%s %s\n",
		serverUrl,
		flags,
		clientToServerPath,
		serverToClientPath,
	)
}

func clientHandleWithYamux(ln net.Listener, httpClient *http.Client, headers []piping_tunnel_util.KeyValue, clientToServerUrl string, serverToClientUrl string) error {
	duplex, err := makeDuplexWithEncryptionAndProgressIfNeed(httpClient, headers, clientToServerUrl, serverToClientUrl, clientSymmetricallyEncrypts, clientSymmetricallyEncryptPassphrase, clientCipherType)
	if err != nil {
		return err
	}
	yamuxSession, err := yamux.Client(duplex, nil)
	if err != nil {
		return err
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		yamuxStream, err := yamuxSession.Open()
		if err != nil {
			return err
		}
		fin := make(chan struct{})
		go func() {
			var buf = make([]byte, clientYamuxBufSize)
			io.CopyBuffer(yamuxStream, conn, buf)
			fin <- struct{}{}
		}()
		go func() {
			var buf = make([]byte, clientYamuxBufSize)
			io.CopyBuffer(conn, yamuxStream, buf)
			fin <- struct{}{}
		}()
		go func() {
			<-fin
			<-fin
			close(fin)
			conn.Close()
			yamuxStream.Close()
		}()
	}
}
