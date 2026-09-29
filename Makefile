# Build/deploy helpers for pmrunner. See README.md for the underlying
# `go build` command and the runner/watcher config directory layout.
#
# DEPLOY_DIR is the live config directory on the VPS -- it holds .env,
# repos.json, and logs, and is separate from this dev clone. Override it
# to deploy somewhere else, e.g.:
#
#   make deploy DEPLOY_DIR=/path/to/other/dir
#
# Both runner and watcher run under systemd (the pmrunner-runner and
# pmrunner-watcher units) since 2026-09-25. `deploy` only installs the new
# binary and restarts those units through the scoped sudoers rule -- it no
# longer manages processes itself.
#
# Set DRY_RUN=1 to print what `deploy` would do without doing it.
#
# `test-fixture` builds a throwaway bare+working git clone pair shaped
# like a PM vault, for internal/runner and internal/watcher's integration
# tests (see internal/testvault). Override its location with
# PMRUNNER_TEST_VAULT_PATH=/some/path -- the tests read the same variable.

BINARY := pmrunner
DEPLOY_DIR ?= /home/obsidian/pm-runner-go
DRY_RUN ?= 0
PMRUNNER_TEST_VAULT_PATH ?= /home/obsidian/pmrunner-go-test-vault

.PHONY: build deploy test-fixture

build:
	go build -o $(BINARY) ./cmd/pmrunner

deploy: build
	@deploy_dir=$$(readlink -f "$(DEPLOY_DIR)"); \
	if [ ! -d "$$deploy_dir" ]; then \
		echo "deploy: $$deploy_dir does not exist" >&2; \
		exit 1; \
	fi; \
	echo "deploy: installing $(BINARY) into $$deploy_dir"; \
	if [ "$(DRY_RUN)" = "1" ]; then \
		echo "[dry-run] cp $(BINARY) $$deploy_dir/.$(BINARY).new"; \
		echo "[dry-run] cp -a $$deploy_dir/$(BINARY) $$deploy_dir/$(BINARY).prev"; \
		echo "[dry-run] chmod 755 $$deploy_dir/.$(BINARY).new"; \
		echo "[dry-run] mv $$deploy_dir/.$(BINARY).new $$deploy_dir/$(BINARY)"; \
		echo "[dry-run] sudo -n systemctl restart pmrunner-runner pmrunner-watcher"; \
	else \
		cp "$(BINARY)" "$$deploy_dir/.$(BINARY).new" && \
		if [ -f "$$deploy_dir/$(BINARY)" ]; then \
			cp -a "$$deploy_dir/$(BINARY)" "$$deploy_dir/$(BINARY).prev"; \
		fi && \
		chmod 755 "$$deploy_dir/.$(BINARY).new" && \
		mv "$$deploy_dir/.$(BINARY).new" "$$deploy_dir/$(BINARY)" && \
		sudo -n systemctl restart pmrunner-runner pmrunner-watcher; \
	fi

test-fixture:
	@work="$(PMRUNNER_TEST_VAULT_PATH)"; \
	bare="$$work.git"; \
	if [ -d "$$work/.git" ]; then \
		echo "test-fixture: $$work already exists, skipping"; \
		exit 0; \
	fi; \
	echo "test-fixture: creating throwaway vault at $$work (bare: $$bare)"; \
	git init --bare "$$bare" && \
	git clone "$$bare" "$$work" && \
	echo "throwaway test vault -- see internal/testvault" > "$$work/README.md" && \
	mkdir -p "$$work/Tasks" && \
	touch "$$work/Tasks/.gitkeep" && \
	git -C "$$work" checkout -B master && \
	git -C "$$work" -c user.email="test-fixture@example.com" -c user.name="test-fixture" add -A && \
	git -C "$$work" -c user.email="test-fixture@example.com" -c user.name="test-fixture" commit -q -m "test-fixture: initial commit" && \
	git -C "$$work" push -q origin HEAD:master
