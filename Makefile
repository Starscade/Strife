.POSIX:


GIT_BRANCH != git branch --show-current 2>/dev/null || echo main
GIT_TAG != git describe --tags --always 2>/dev/null || echo v0
INSTALL_DIR = $(HOME)/.local/bin
NAME != basename "$$(pwd)"
LDFLAGS = -s -w -X 'main.Version=$(GIT_TAG)-$(GIT_BRANCH)'

INSTALL_PATH = $(INSTALL_DIR)/$(NAME)


all:

	@                            \
	mkdir -p "$(INSTALL_DIR)" && \
	go mod tidy               && \
	go fmt                    && \
	CGO_ENABLED=0                \
	go build                     \
	  -ldflags="$(LDFLAGS)"      \
	  -o "$(INSTALL_PATH)"       \
	  -trimpath                  \
	  -v                         \
	  -x


dock:

	@                                                     \
	docker build --no-cache                               \
	             --build-arg NAME=$(NAME)                 \
	             --build-arg INSTALL_PATH=$(INSTALL_PATH) \
	             --output "$(INSTALL_DIR)" .


dock-img:

	@docker build --no-cache -t $(NAME) .


dock-run:

	@                                    \
	docker rm  -f         $(NAME)     && \
	docker run --env-file .env           \
	           --name     $(NAME)        \
	           --restart  unless-stopped \
	                      $(NAME)


test:

	@go vet --race && go test --json


