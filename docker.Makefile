# dubplate version: v0.8.1

dockerfile:
	$(MAKE) Dockerfile.$(APP_NAME)

Dockerfile.%:
	sed 's/{{APP_NAME}}/$(subst Dockerfile.,,$@)/g' Dockerfile.template > $(BUILD_DIR)/$@

image: Dockerfile.$(APP_NAME) check-docker-username
	docker build \
		--tag $(DOCKER_USERNAME)/$(APP_NAME):$(VERSION) \
		-f $(BUILD_DIR)/Dockerfile.$(APP_NAME) \
		$(BUILD_DIR)

images: $(COMPONENTS:=-image)

$(COMPONENTS:=-image):
	$(MAKE) image \
		APP_NAME=$(@:-image=)

check-docker-username:
ifndef DOCKER_USERNAME
	$(error DOCKER_USERNAME var not defined)
endif
