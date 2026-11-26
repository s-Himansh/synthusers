# Source this file to get the project-local Go toolchain:
#   source /home/z/my-project/scripts/goenv.sh
export PATH="/home/z/my-project/.toolchain/go/bin:$PATH"
export GOPATH="/home/z/my-project/.toolchain/gopath"
export GOCACHE="/home/z/my-project/.toolchain/gocache"
export GOMODCACHE="$GOPATH/pkg/mod"
export GOFLAGS=-mod=mod
export CGO_ENABLED=0
