# Fugitive3dDeployment
Program to simplify deploying dedicated servers to Digital Ocean

This allows us to:

1. provision a virtual machine (droplet) with an ssh key installed on the root user account
2. grab that droplet's public IP
3. poll it until linux is responding to ssh login requests
4. log in, punch holes in the firewall
5. copy over the entire docker context for the server
6. decompress and build the container
7. run the container, bringing the server online


## Getting started

The first thing to edit is the config file. (almost) All your user settings are going to be in this file.

```
{
	"_comment": "This is an overall local user config file for fugitive server deployment.",
    "droplet": {
    	"name": "fugitive-droplet",
        "sizeslug": "s-3vcpu-1gb",
        "imageslug": "docker-18-04",
        "region": "sfo2",
        "tag": "automated",
        "sshkeyid": 123456
    },
    "remoteusername": "root",
    "sshprivatekey": "/Users/batman/.ssh/id_rsa_digitalocean",
    "f3d_repo_root": "/Users/batman/dev/Fugitive3D",
    "zipfilename": "fugitive-crap.zip",
    "godot_linux_server_url": "https://downloads.tuxfamily.org/godotengine/3.2.1/Godot_v3.2.1-stable_linux_server.64.zip",
    "godot_binary_path": "/Applications/Godot.app/Contents/MacOS/Godot"
}
``` 

Let's go over the fields and what they mean.

**Droplet**

This section allows settings for the droplet being provisioned.

* name - the default is probably fine
* sizeslug - the DO string representation of one of their VM shapes
* imageslug - Ubuntu 18.04 with docker, the default is fine. We just need Ubuntu and docker.
* region - region short name for one of DO's regions
* tag - This is a custom tag we can apply to the droplet after creation. It allows for easy searching/manipulation of droplets we created with automation.
* sshkeyid - This is an integer. After you upload an SSH public key to DO, they give it an ID. This is that ID. It's the key we will copy over to the root account on the droplet.

**Additional Settings**

The settings at the root level here describe some details of your local workspace.

* remoteusername - This is the user account on the droplet. Use the root default for now.
* sshprivatekey - This is an absolute path to your ssh _private key file_ on your local filesystem. This is the private key corresponding with the public one you uploaded to DO already. It cannot be protected with a passphrase to be used with this script.
* f3d_repo_root - This is the absolute path to the Fugitive3D repository on your local filesystem.
* zipfilename - This is just a temporary name so you can leave it default. This deploy script will create this file wherever it is run. The file can be safely deleted locally after you spin up a droplet.
* godot_linux_server_url - This is a URL the droplet is going to download the linux godot server binary from.
* godot_binary_path - This is the absolute path on your local filesystem to godot. Used for building on the CLI.

**Environment Variables**

Make sure to set the environment variable `FUGITIVE_DO_TOKEN` to your DO API key string. We keep this in your local environment and out of the config file for security's sake.

## Updating Docker Context
The directory `extras/deploy/container` needs to have four files in it:

* the linux server .pck
* the godot linux binary
* the Dockerfile
* the run.sh script

The former two aren't in this repository because you have to build/place them in this directory yourself.

## Deploying Everything
Now just build `deploy.go`, do a `deploy` and watch. 