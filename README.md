# PStream implementation based on the [MPQUIC](https://github.com/qdeconinck/mp-quic) project

**Please read https://multipath-quic.org/2017/12/09/artifacts-available.html to figure out how to setup the code.**

<img src="docs/quic.png" width=303 height=124>

[![Godoc Reference](https://img.shields.io/badge/godoc-reference-blue.svg?style=flat-square)](https://godoc.org/github.com/lucas-clemente/pstream)
[![Linux Build Status](https://img.shields.io/travis/lucas-clemente/pstream/master.svg?style=flat-square&label=linux+build)](https://travis-ci.org/lucas-clemente/pstream)
[![Windows Build Status](https://img.shields.io/appveyor/ci/lucas-clemente/pstream/master.svg?style=flat-square&label=windows+build)](https://ci.appveyor.com/project/lucas-clemente/pstream/branch/master)
[![Code Coverage](https://img.shields.io/codecov/c/github/lucas-clemente/pstream/master.svg?style=flat-square)](https://codecov.io/gh/lucas-clemente/pstream/)


## Guides

We currently support Go 1.9+.

Installing and updating dependencies:

    go get -t -u ./...

Running tests:

    go test ./...

### Running the example server

    go run example/main.go -www /var/www/

Using the `quic_client` from chromium:

    quic_client --host=127.0.0.1 --port=6121 --v=1 https://quic.clemente.io

Using Chrome:

    /Applications/Google\ Chrome.app/Contents/MacOS/Google\ Chrome --user-data-dir=/tmp/chrome --no-proxy-server --enable-quic --origin-to-force-quic-on=quic.clemente.io:443 --host-resolver-rules='MAP quic.clemente.io:443 127.0.0.1:6121' https://quic.clemente.io

### QUIC without HTTP/2

Take a look at [this echo example](example/echo/echo.go).

### Using the example client

    go run example/client/main.go https://clemente.io

## Usage

### As a server

See the [example server](example/main.go) or try out [Caddy](https://github.com/mholt/caddy) (from version 0.9, [instructions here](https://github.com/mholt/caddy/wiki/QUIC)). Starting a QUIC server is very similar to the standard lib http in go:

```go
http.Handle("/", http.FileServer(http.Dir(wwwDir)))
h2quic.ListenAndServeQUIC("localhost:4242", "/path/to/cert/chain.pem", "/path/to/privkey.pem", nil)
```

### As a client

See the [example client](example/client/main.go). Use a `h2quic.RoundTripper` as a `Transport` in a `http.Client`.

```go
http.Client{
  Transport: &h2quic.RoundTripper{},
}
```


