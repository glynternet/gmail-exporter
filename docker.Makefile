# dubplate version: v0.8.1

DOCKER_IMAGE ?= $(DOCKER_USERNAME)/$(APP_NAME):$(VERSION)

dockerfile:
	$(MAKE) Dockerfile.$(APP_NAME)

Dockerfile.%: $(BUILD_DIR) binary
	sed 's/{{APP_NAME}}/$(subst Dockerfile.,,$@)/g' Dockerfile.template > $(BUILD_DIR)/$@

image: Dockerfile.$(APP_NAME) check-docker-username
	docker build \
		--tag $(DOCKER_IMAGE) \
		-f $(BUILD_DIR)/Dockerfile.$(APP_NAME) \
		$(BUILD_DIR)

images: $(COMPONENTS:=-image)

$(COMPONENTS:=-image):
	$(MAKE) image \
		APP_NAME=$(@:-image=)

push-image: image
	docker push \
		$(DOCKER_IMAGE)

push-images: $(COMPONENTS:=-push-image)

$(COMPONENTS:=-push-image):
	$(MAKE) push-image \
		APP_NAME=$(@:-push-image=)

check-docker-username:
ifndef DOCKER_USERNAME
	$(error DOCKER_USERNAME var not defined)
endif
