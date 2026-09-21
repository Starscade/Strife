.POSIX:


GIT_TAG := $(shell git describe --tags)
LDFLAGS := -s -w -X 'main.Version=$(GIT_TAG)'


all:

	@mkdir -p ~/.local/bin   && \
	 go mod tidy             && \
	 go fmt                  && \
	 CGO_ENABLED=0a             \
	 go build                   \
	   -ldflags="$(LDFLAGS)"    \
	   -o ~/.local/bin/Strife   \
	   -v                       \
	   -x


dock:

	@docker build --no-cache -t strife .


