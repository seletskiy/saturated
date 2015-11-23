saturated is a daemon which will build and install packages, most usable as a
post-receive hook in repository.

Step by step, it will do the following:

* listen for requests on `/v1/build/<repo-url>`;

* clone `<repo-url>` or update local copy;

* run build command, by default `makepkg`;

* run install command, by default nothing, but install command expected
  to be found at `/usr/lib/saturated/install-package` path;

Most usable case is provide an install command, which will automatically upload
package to the remote package repository. It's specific to infrastructure, so
upload script is not in the packaging and you must provide it by yourself.

Logs will be outputted in realtime back to the requesting client.

You can pass additional environment variable to build command via GET parameter
`environ`: 
```
/v1/build/<repo-url>?environ=KEY=value
```

# Installation

## Archlinux

PKGBUILD available at AUR:

https://aur4.archlinux.org/packages/saturated

## go get

```
go get github.com/seletskiy/saturated
```

# Typical configuration

1. Install package as specified above (use prepared PKBUILD).
2. Package will create makepkg user with sudo-privileges.
3. Init SSH key-pair using `sudo -u makepkg ssh-keygen`.
4. Copy public part of the SSH key to required source-code repositories (if
   needed).
5. Prepare and test installation script, that will be executed in the working
   directory after successfull build.
6. Copy installation script into /usr/lib/saturated/install-package (make sure
   it has exectable flag).
7. Run saturated using systemctl service: `systemctl start saturated`, it will
   listen on the address `:8080`.
