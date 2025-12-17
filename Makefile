config.yml:
	curl -sSfL https://api.tinfoil.sh/api/config/router -o config.yml.tmp && mv config.yml.tmp config.yml

build: config.yml
	go build -o proxy

clean:
	rm -f proxy config.yml

.PHONY: build clean
