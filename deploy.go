package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/exec"
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
	Remoteuser      string `json:"remoteusername"`
	SSHPrivKey      string `json:"sshprivatekey"`
	F3DRepoRoot     string `json:"f3d_repo_root"`
	ZipFileName     string `json:"zipfilename"`
	GodotBinaryPath string `json:"godot_binary_path"`
	GodotServerUrl  string `json:"godot_linux_server_url"`
}

// relative to the F3DRepoRoot
const DOCKERCONTEXT = "extras/deploy/container"

// Parse config.json off the disk and return a Config struct.
func RenderConfig(file string) Config {
	var parsed Config
	cfgFile, err := os.Open(file)
	defer cfgFile.Close()
	if err != nil {
		log.Println(err)
		log.Fatal("Error loading your configuration file!")
	}
	parser := json.NewDecoder(cfgFile)
	parser.Decode(&parsed)

	// These need to be absolute when used
	if !filepath.IsAbs(parsed.GodotBinaryPath) {
		parsed.GodotBinaryPath, _ = filepath.Abs(parsed.GodotBinaryPath)
	}
	if !filepath.IsAbs(parsed.SSHPrivKey) {
		parsed.SSHPrivKey, _ = filepath.Abs(parsed.SSHPrivKey)
	}
	if !filepath.IsAbs(parsed.F3DRepoRoot) {
		parsed.F3DRepoRoot, _ = filepath.Abs(parsed.F3DRepoRoot)
	}

	return parsed
}

// Reduce droplet definition down to providing a couple parameters.
// SSH key here is an integer ID which DO gives to your public key after you upload it.
func newDroplet(c Config) *godo.DropletCreateRequest {
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
func runFileCopy(config ssh.ClientConfig, host, localfile, remotefile string) {

	addr := fmt.Sprintf("%s:22", host)
	client, err := ssh.Dial("tcp", addr, &config)
	if err != nil {
		log.Fatalf("unable to connect to [%s]: %v", addr, err)
	}
	defer client.Close()

	log.Println("We are connected for SCP.")
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

	log.Printf("Remote file:", remotefile)
	log.Printf("Local file:", localfile)
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

// Run a single command on a remote system over SSH. STDOUT is returned in a string, along with err.
func runSSHCmd(config ssh.ClientConfig, host, command string) (string, error) {
	var output string
	var err error
	target := fmt.Sprintf("%s:22", host)
	client, err := ssh.Dial("tcp", target, &config)

	if err != nil {
		log.Printf("Failed to dial: %s", err)
		return output, err
	}

	// Each client connection can support multiple interactive sessions,
	// represented by a Session.
	session, err := client.NewSession()
	if err != nil {
		log.Printf("Failed to create session:: %s", err)
		return output, err
	}
	defer session.Close()

	var b bytes.Buffer
	session.Stdout = &b
	if err := session.Run(command); err != nil {
		log.Printf("Failed to run command '%s': %s", command, err)
		return output, err
	}
	output = b.String()

	return output, err
}

func addFileToZip(zipWriter *zip.Writer, filename string) error {

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
	// This is the temporary zip file, doesn't matter where it's placed locally.
	zipAbsPath, _ := filepath.Abs(config.ZipFileName)

	newZipFile, err := os.Create(config.ZipFileName)
	if err != nil {
		log.Fatal(err)
	}
	defer newZipFile.Close()

	zipWriter := zip.NewWriter(newZipFile)
	defer zipWriter.Close()

	// grab all the files in the repo's docker context.
	files, err := ioutil.ReadDir(filepath.Join(config.F3DRepoRoot, DOCKERCONTEXT))
	if err != nil {
		log.Fatal(err)
	}

	// remove any directories we found
	justFiles := make([]os.FileInfo, 0, len(files))

	log.Println("Files to be included in the docker context:")
	for _, f := range files {
		// Don't allow recursion into directories for now.
		if f.IsDir() {
			log.Printf("Ignoring directory: %s\n", f.Name())
			continue
		}
		justFiles = append(justFiles, f)
		log.Printf("%-11s%-12d%-2s\n", f.Mode(), f.Size(), f.Name())
	}

	// Add files to zip
	log.Println("smooshing files together...")
	for _, file := range justFiles {
		abspath := filepath.Join(config.F3DRepoRoot, DOCKERCONTEXT, file.Name())
		log.Printf("File added to archive: %s\n", abspath)
		if err = addFileToZip(zipWriter, abspath); err != nil {
			log.Println(err)
			log.Fatal(err)
		}
	}
	//TODO: Remove the local copy of this zip in a cleanup step.
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

	opt := &godo.ListOptions{
		Page:    1,
		PerPage: 50,
	}
	keys, _, _ := client.Keys.List(ctx, opt)
	log.Printf("SSH keys on your account: %d\n", len(keys))
	for _, v := range keys {
		log.Printf("ID: %d, Fingerprint:%s\n", v.ID, v.Fingerprint)
	}
	return ctx, client
}

// Create a droplet and poll until the public IP is known, returning it on a channel.
// Then poll for linux to be up and ready.
func provisionDroplet(config Config, ctx context.Context, client godo.Client, req *godo.DropletCreateRequest, ch chan<- godo.Droplet) {
	log.Println("Droplet being created with parameters:", req)
	droplet, _, _ := client.Droplets.Create(ctx, req)

	log.Printf("Droplet created with ID:%d\n", droplet.ID)

	opt := &godo.ListOptions{
		Page:    1,
		PerPage: 20,
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

// Attempt to ssh to the target and run 'uptime'. Return true if it doesn't indicate an error.
func isDropletUp(config ssh.ClientConfig, host string) bool {
	log.Println("Attempting to see if the droplet is reachable...")
	_, err := runSSHCmd(config, host, "uptime")
	if err != nil {
		return false
	}
	return true
}

func buildServerLocally(config Config) {
	// Location of the .pck file is relative to the repo root.
	// It's a showstopper if this cannot build the .pck
	log.Println("Local server build is being kicked off, output will be shown below when finished.")
	cmd := exec.Command(config.GodotBinaryPath,
		"--path",
		config.F3DRepoRoot,
		"--export",
		"Server - Linux",
		filepath.Join(config.F3DRepoRoot, DOCKERCONTEXT, "data.pck"))
	stdoutStderr, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatal(string(stdoutStderr))
	}
	log.Println(string(stdoutStderr), err)
}

func main() {
	// Provide a command line tool for interaction with the various layers of Fugitive infrastructure
	// - The virtual machine provisioning (droplets)
	// - The linux system installed on said droplet
	// - The applications running on the droplet. In this case, Fugitive related applcations.

	var build bool
	var now bool
	var configlocation string

	flag.BoolVar(&build, "build", false, "Build the linux server, move it into the docker context, and exit.")
	flag.BoolVar(&now, "now", false, "Run an entire deployment to a new droplet.")
	flag.StringVar(&configlocation, "c", "config.json", "path to deployment config JSON file")
	flag.Parse()

	if len(os.Args) < 2 {
		flag.PrintDefaults()
	}

	deployConfig := RenderConfig(configlocation)

	// Just build the linux server pck locally, then bail.
	if build {
		buildServerLocally(deployConfig)
		return
	}

	// This exits. It prevents accidental deployments. "-now" is an "are you sure?"
	if !now {
		return
	}

	sshConfig := ssh.ClientConfig{
		User: deployConfig.Remoteuser,
		Auth: []ssh.AuthMethod{
			publicKey(deployConfig.SSHPrivKey),
		},
		// This is a brand new server, so new host keys. Default to 'fuck it'.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	ctx, client := grabContext(os.Getenv("FUGITIVE_DO_TOKEN"))

	// These three cases are:
	// A set of API calls to DO to spin up a droplet
	// A command to zip up all local files needed to build the docker image
	// A timeout of 60 seconds for both channels
	dropletChan := make(chan godo.Droplet, 1)
	zipChan := make(chan string, 1)
	createRequest := newDroplet(deployConfig)

	go provisionDroplet(deployConfig, ctx, *client, createRequest, dropletChan)
	go createDockerZIP(deployConfig, zipChan)

	var publicIP, zipFile string

	for i := 0; i < 2; i++ {
		select {
		case dropletData := <-dropletChan:
			// droplet data (with reachable IP), straight out of the DO API reply
			log.Println("droplet finished provisioning")
			log.Println(dropletData)
			publicIP = dropletData.Networks.V4[0].IPAddress
			log.Printf("Droplet public IP:%s\n", publicIP)
		case zipComplete := <-zipChan:
			// local zip file created, ready to copy.
			log.Println(zipComplete)
			zipFile = zipComplete
		case <-time.After(60 * time.Second):
			// This shouldn't take longer than a minute.
			log.Println("timeout reached when provisioning droplet and/or zipping docker context.")
		}
	}

	// We can't do anything with the droplet until it's reachable via ssh.
	reachable := false
	retryCount := 12
	retryInterval := 10 * time.Second
	log.Println("Attempting to see if the droplet is reachable...")
	for i := 0; i < retryCount; i++ {
		response, err := runSSHCmd(sshConfig, publicIP, "uptime")
		if err != nil {
			time.Sleep(retryInterval)
			log.Println("Still trying...")
		} else {
			log.Printf("Server uptime: %s", response)
			reachable = true
			break
		}
	}

	if reachable {
		remotefile := fmt.Sprintf("/root/%s", deployConfig.ZipFileName)
		runFileCopy(sshConfig, publicIP, zipFile, remotefile)
	} else {
		l := fmt.Sprintf("Droplet wasn't reachable after %d attempts.", retryCount)
		log.Fatal(l)
	}

	// oh god....where are we...what is the meaning of this all?

	// This all assumes we land in our homedir on the remote end, so /root.
	prepCommands := []string{
		"ufw allow 31000/tcp",
		"ufw allow 31000/udp",
		"apt install unzip",
		fmt.Sprintf("unzip %s", deployConfig.ZipFileName),
		fmt.Sprintf("curl %s -o linux.zip", deployConfig.GodotServerUrl),
		fmt.Sprintf("unzip linux.zip"),
		"docker build -t fugitive-server .",
		"docker run -d --name fugitive-server --net=host fugitive-server",
		"docker ps -a",
		"docker logs fugitive-server",
	}

	for _, cmd := range prepCommands {
		log.Printf("Executing remote command: '%s'", cmd)
		output, _ := runSSHCmd(sshConfig, publicIP, cmd)
		log.Println(output)
	}

	// log.Printf("ID of droplet created in this run: %d", )
	log.Println("Deployment complete!")
	// ssh command, just in case you want to ssh in directly on the command line.
	log.Printf("\nssh -i %s %s@%s\n", deployConfig.SSHPrivKey, deployConfig.Remoteuser, publicIP)

	// uncomment to delete the droplet.
	// client.Droplets.DeleteByTag(ctx, deployConfig.Droplet.Tag)
}
