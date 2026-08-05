EXEC=hepmjerenja
CONTAINER_NAME=hepmjerenja
IMAGE_NAME=hepmjerenja
REMOTE_IMAGE=gitea.ekirin.com/eden/$(IMAGE_NAME)
VERSION_TAG=$$(date +%y%m%d.%H%M)

run:
	@air

.PHONY: build
build:
	@go build -ldflags "-s -w" -o ./build/${EXEC} ./app/.

migrate:
	@go run ./app migrate up

migrate-down:
	@go run ./app migrate down

migrate-status:
	@go run ./app migrate status

upgrade-packages:
	@go get -u ./...

clean:
	- @docker rm $(CONTAINER_NAME)
	- docker images | grep '$(IMAGE_NAME) ' | awk '{print $$1 ":" $$2}' | xargs docker rmi
	- docker images | grep '$(REMOTE_IMAGE) ' | awk '{print $$1 ":" $$2}' | xargs docker rmi

docker-run:
	@test -f config.ini || { echo "config.ini ne postoji — kopirajte config.ini.example"; exit 1; }
	- @docker run -d \
	    --name $(CONTAINER_NAME) \
		-v $(CURDIR)/config.ini:/app/config.ini:ro,z \
		-v hepmjerenja-data:/app/data \
		-p 8000:8000 \
		$(CONTAINER_NAME)

docker-build: clean
	@docker \
		build \
		--no-cache \
		--progress plain \
		--tag $(IMAGE_NAME):$(VERSION_TAG) \
		--tag $(IMAGE_NAME):latest \
		--tag $(REMOTE_IMAGE):$(VERSION_TAG) \
		--tag $(REMOTE_IMAGE):latest \
		.

docker-push:
	@git tag -a "$(VERSION_TAG)" -m "$(VERSION_TAG)"
	@git push origin "$(VERSION_TAG)"
	@docker push $(REMOTE_IMAGE):$(VERSION_TAG)
	@docker push $(REMOTE_IMAGE):latest
