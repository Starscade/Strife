.POSIX:


all:

	@./install.sh


dock:

	@docker build --no-cache -t strife .


