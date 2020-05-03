package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"time"

	"fmt"

	"io/ioutil"
	"log"
	"strings"

	"github.com/digitalocean/godo"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type Config struct {
	Droplet struct {
		Name   string `json:"name"`
		Size   string `json:"sizeslug"`
		Image  string `json:"imageslug"`
		Region string `json:"region"`
		Tag    string `json:"tag"`
		SSHKey int    `json:"sshkeyid"`
	} `json:"droplet"`
	Remoteuser    string `json:"remoteusername"`
	SSHPrivKey    string `json:"sshprivatekey"`
	DockerContext string `json:"dockercontext"`
	ZipFileName   string `json:"zipfilename"`
}

func RenderConfig(file string) Config {
	var parsed Config
	cfgFile, err := os.Open(file)
	defer cfgFile.Close()
	if err != nil {
		fmt.Println(err)
		log.Fatal("Error loading your configuration file!")
	}
	parser := json.NewDecoder(cfgFile)
	parser.Decode(&parsed)

	// These need to be absolute when used
	if !filepath.IsAbs(parsed.DockerContext) {
		parsed.DockerContext, _ = filepath.Abs(parsed.DockerContext)
	}
	if !filepath.IsAbs(parsed.SSHPrivKey) {
		parsed.SSHPrivKey, _ = filepath.Abs(parsed.SSHPrivKey)
	}

	return parsed
}

// Reduce droplet definition down to providing a couple parameters.
// SSH key here is an integer ID which DO gives to your public key after you upload it.
func NewDroplet(c Config) *godo.DropletCreateRequest {
	return &godo.DropletCreateRequest{
		Name:   c.Droplet.Name,
		Region: c.Droplet.Region,
		Size:   c.Droplet.Size,
		Image: godo.DropletCreateImage{
			Slug: c.Droplet.Image,
		},
		SSHKeys: []godo.DropletCreateSSHKey{
			godo.DropletCreateSSHKey{ID: c.Droplet.SSHKey},
		},
		IPv6: false,
		Tags: []string{c.Droplet.Tag}}
}

// Load in an SSH private key from a local absolute path.
// Return the corresponding private key object for use in auth.
func publicKey(path string) ssh.AuthMethod {

	if strings.HasSuffix(path, ".pub") {
		log.Fatal("Use you _private_ key. Hint: it usually doesn't end in .pub.")
	}

	// todo: this is janky
	key, err := ioutil.ReadFile(path)
	if err != nil {
		panic(err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		panic(err)
	}
	return ssh.PublicKeys(signer)
}

// This is just an SCP over to the remote host using our established creds in &config.
func runFileCopy(config ssh.ClientConfig, host string, localfile string, remotefile string) {

	addr := fmt.Sprintf("%s:22", host)
	client, err := ssh.Dial("tcp", addr, &config)
	if err != nil {
		log.Fatalf("unable to connect to [%s]: %v", addr, err)
	}
	defer client.Close()

	fmt.Println("We are connected for SCP.")
	pktsize := 1 << 15

	c, err := sftp.NewClient(client, sftp.MaxPacket(pktsize))
	if err != nil {
		log.Fatalf("unable to start sftp subsytem: %v", err)
	}
	defer c.Close()

	// remote end: create new, overwrite existing
	w, err := c.OpenFile(remotefile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	fmt.Println("Remote file:", remotefile)
	fmt.Println("Local file:", localfile)
	// local end
	f, err := os.Open(localfile)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	st, err := os.Stat(localfile)
	if err != nil {
		log.Fatal(err)
	}

	size := st.Size()

	// This will overwrite anything already on the remote path at that name.
	log.Printf("writing %v bytes", size)
	log.Print("This might take a couple minutes...")
	t1 := time.Now()
	n, err := io.Copy(w, io.LimitReader(f, size))
	if err != nil {
		log.Fatal(err)
	}

	// This is more of an FYI that we lost data somewhere. Try again.
	if n != size {
		log.Fatalf("copy: expected %v bytes, got %d", size, n)
	}
	log.Printf("wrote %v bytes in %s", size, time.Since(t1))
}

// Run a single command on a remote system over SSH. STDOUT is returned in a string.
func runSSHCmd(config ssh.ClientConfig, host string, command string) string {

	target := fmt.Sprintf("%s:22", host)
	client, err := ssh.Dial("tcp", target, &config)
	if err != nil {
		log.Fatal("Failed to dial: ", err)
	}

	// Each client connection can support multiple interactive sessions,
	// represented by a Session.
	session, err := client.NewSession()
	if err != nil {
		log.Fatal("Failed to create session: ", err)
	}
	defer session.Close()

	var b bytes.Buffer
	session.Stdout = &b
	if err := session.Run(command); err != nil {
		log.Fatal("Failed to run: " + err.Error())
	}
	var result = b.String()

	return result
}

func AddFileToZip(zipWriter *zip.Writer, filename string) error {

	fileToZip, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer fileToZip.Close()

	// Get the file information
	info, err := fileToZip.Stat()
	if err != nil {
		return err
	}

	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}

	header.Method = zip.Deflate

	writer, err := zipWriter.CreateHeader(header)
	if err != nil {
		return err
	}

	_, err = io.Copy(writer, fileToZip)
	return err
}

// Zip the entire docker context (directory) so we can copy it to the remote end.
func createDockerZIP(config Config, ch chan<- string) {

	// create a new local file c.zipfilename
	// TODO: control the path of this file.
	zipAbsPath, _ := filepath.Abs(config.ZipFileName)

	newZipFile, err := os.Create(config.ZipFileName)
	if err != nil {
		log.Fatal(err)
	}
	defer newZipFile.Close()

	zipWriter := zip.NewWriter(newZipFile)
	defer zipWriter.Close()

	// grab all the files in the repo's docker context.
	files, err := ioutil.ReadDir(config.DockerContext)
	if err != nil {
		log.Fatal(err)
	}

	// remove any directories we found
	justFiles := make([]os.FileInfo, 0, len(files))

	fmt.Println("Files to be included in the docker context:")
	for _, f := range files {
		// Don't allow recursion into directories for now.
		if f.IsDir() {
			fmt.Printf("Ignoring directory: %s\n", f.Name())
			continue
		}
		justFiles = append(justFiles, f)
		fmt.Printf("%-11s%-12d%-2s\n", f.Mode(), f.Size(), f.Name())
	}

	// Add files to zip
	fmt.Println("smooshing files together...")
	for _, file := range justFiles {
		abspath := filepath.Join(config.DockerContext, file.Name())
		fmt.Printf("File added to archive: %s\n", abspath)
		if err = AddFileToZip(zipWriter, abspath); err != nil {
			fmt.Println(err)
			log.Fatal(err)
		}
	}
	ch <- zipAbsPath
}

func grabContext(token string) (context.Context, *godo.Client) {
	// verify the user and their token are OK to proceed, return a context and client.
	client := godo.NewFromToken(token)
	ctx := context.TODO()

	// Semantic validation. Are you an actual user in our DO group? Did you get your token right?
	_, _, err := client.Account.Get(ctx)
	if err != nil {
		log.Fatal(err)
	}
	return ctx, client
}

func provisionDroplet(config Config, ctx context.Context, client godo.Client, req *godo.DropletCreateRequest, ch chan<- godo.Droplet) {
	// Create a droplet and poll until the public IP is known, returning it on a channel.
	// Then poll for linux to be up and ready.
	fmt.Println("Droplet being created with parameters:", req)
	droplet, _, _ := client.Droplets.Create(ctx, req)

	fmt.Printf("Droplet created with ID:%d\n", droplet.ID)

	opt := &godo.ListOptions{
		Page:    1,
		PerPage: 10,
	}

	// Poll the created droplet for the IP address given to it.
	// The droplet is only useful to us after it has a public IP allocated.
	for {
		time.Sleep(5 * time.Second)
		droplets, _, _ := client.Droplets.ListByTag(ctx, config.Droplet.Tag, opt)
		for _, dropletData := range droplets {
			if len(dropletData.Networks.V4) < 1 {
				continue
			}
			publicIP := dropletData.Networks.V4[0].IPAddress
			if publicIP != "" {

				ch <- dropletData
				break
			}
		}
	}
}

func isDropletUp(config ssh.ClientConfig, host string) bool {
	// at this point, we need to poll to see if we can run userland commands
	// so we can start copying stuff over
	fmt.Println("Attempting to see if the droplet is reachable...")
	for {
		response := runSSHCmd(config, host, "uptime")
		if response != "" {
			return true
		}
		time.Sleep(5 * time.Second)
		fmt.Println("Still trying...")
	}
}

func main() {
	// Provide a command line tool for interaction with the various layers of Fugitive infrastructure
	// - The virtual machine provisioning (droplets)
	// - The linux system installed on said droplet
	// - The applications running on the droplet. In this case, Fugitive related applcations.

	var configlocation string
	var destroyDroplet bool

	flag.StringVar(&configlocation, "c", "config.json", "path to deployment config JSON file")
	flag.BoolVar(&destroyDroplet, "d", false, "Destroy all droplets with tag 'automated'")

	deployConfig := RenderConfig(configlocation)
	sshConfig := ssh.ClientConfig{
		User: deployConfig.Remoteuser,
		Auth: []ssh.AuthMethod{
			publicKey(deployConfig.SSHPrivKey),
		},
		// This is a brand new server, so new host keys. Default to 'fuck it'.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	dropletChan := make(chan godo.Droplet, 1)
	zipChan := make(chan string, 1)

	ctx, client := grabContext(os.Getenv("FUGITIVE_DO_TOKEN"))

	createRequest := NewDroplet(deployConfig)
	go provisionDroplet(deployConfig, ctx, *client, createRequest, dropletChan)
	go createDockerZIP(deployConfig, zipChan)

	var publicIP, zipFile string

	for i := 0; i < 2; i++ {
		select {
		case dropletData := <-dropletChan:
			// droplet data (with reachable IP), straight out of the DO API reply
			fmt.Println("droplet finished provisioning")
			fmt.Println(dropletData)
			publicIP = dropletData.Networks.V4[0].IPAddress
			fmt.Printf("Droplet public IP:%s\n", publicIP)
		case zipComplete := <-zipChan:
			// local tar file created, ready to copy.
			fmt.Println(zipComplete)
			zipFile = zipComplete
		case <-time.After(60 * time.Second):
			// This shouldn't take longer than a minute.
			fmt.Println("timeout reached when provisioning droplet and/or zipping docker context.")
		}
	}
	var reachable bool
	for i := 0; i < 10; i++ {
		status := isDropletUp(sshConfig, publicIP)
		if status {
			reachable = true
			break
		}
		fmt.Println("Still trying to reach the droplet via ssh...")
	}
	if reachable {
		remotefile := fmt.Sprintf("/root/%s", deployConfig.ZipFileName)
		runFileCopy(sshConfig, publicIP, zipFile, remotefile)
	} else {
		log.Fatal("IDK, wtf!?!")
	}

	// oh god....where are we...what is the meaning of this all?

	// This all assumes we land in our homedir, so /root.
	unzipcmd := fmt.Sprintf("unzip %s", deployConfig.ZipFileName)
	prepCommands := []string{
		"ufw allow 31000/tcp",
		"ufw allow 31000/udp",
		"apt install unzip",
		unzipcmd,
		"docker build -t fugitive-server .",
		"docker run -d --name fugitive-server --net=host fugitive-server",
	}

	for _, cmd := range prepCommands {
		fmt.Printf("Executing remote command: '%s'", cmd)
		output := runSSHCmd(sshConfig, publicIP, cmd)
		fmt.Println(output)
	}

	fmt.Println("Deployment complete!")
	// ssh command, just in case you want to ssh in directly on the command line.
	fmt.Printf("\nssh -i %s %s@%s\n", deployConfig.SSHPrivKey, deployConfig.Remoteuser, publicIP)

	// uncomment to delete the droplet.
	// client.Droplets.DeleteByTag(ctx, deployConfig.Droplet.Tag)
}
