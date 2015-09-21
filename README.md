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

# Installation

## Archlinux

PKGBUILD available at AUR:

https://aur4.archlinux.org/packages/saturated

## go get

```
go get github.com/seletskiy/saturated
```
