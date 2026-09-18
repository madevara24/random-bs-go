# Build/deploy helpers for pmrunner. See README.md for the underlying
# `go build` command and the runner/watcher config directory layout.
#
# DEPLOY_DIR is the live config directory on the VPS -- it holds .env,
# repos.json, and logs, and is separate from this dev clone. Override it
# to deploy somewhere else, e.g.:
#
#   make deploy DEPLOY_DIR=/path/to/other/dir
#
# Neither runner nor watcher runs under systemd today; both are launched
# by hand with nohup. `deploy` finds their PIDs itself (matched by
# command line and working directory) rather than assuming a service
# manager.
#
# Set DRY_RUN=1 to print what `deploy` would do without doing it.

BINARY := pmrunner
DEPLOY_DIR ?= /home/obsidian/pm-runner-go
DRY_RUN ?= 0

.PHONY: build deploy

build:
	go build -o $(BINARY) ./cmd/pmrunner

deploy: build
	@deploy_dir=$$(readlink -f "$(DEPLOY_DIR)"); \
	if [ ! -d "$$deploy_dir" ]; then \
		echo "deploy: $$deploy_dir does not exist" >&2; \
		exit 1; \
	fi; \
	stopped=0; \
	for mode in runner watcher; do \
		pids=""; \
		for pid in $$(pgrep -f "pmrunner $$mode\$$"); do \
			pid_cwd=$$(readlink -f "/proc/$$pid/cwd" 2>/dev/null); \
			if [ "$$pid_cwd" = "$$deploy_dir" ]; then \
				pids="$$pids $$pid"; \
			fi; \
		done; \
		if [ -n "$$pids" ]; then \
			echo "deploy: stopping $$mode (pid:$$pids)"; \
			if [ "$(DRY_RUN)" = "1" ]; then \
				echo "[dry-run] kill$$pids"; \
			else \
				kill $$pids; \
				stopped=1; \
			fi; \
		else \
			echo "deploy: no running $$mode found in $$deploy_dir"; \
		fi; \
	done; \
	if [ "$$stopped" = "1" ]; then sleep 2; fi; \
	echo "deploy: installing $(BINARY) into $$deploy_dir"; \
	if [ "$(DRY_RUN)" = "1" ]; then \
		echo "[dry-run] cp $(BINARY) $$deploy_dir/.$(BINARY).new && mv $$deploy_dir/.$(BINARY).new $$deploy_dir/$(BINARY)"; \
	else \
		cp $(BINARY) "$$deploy_dir/.$(BINARY).new" && \
		mv "$$deploy_dir/.$(BINARY).new" "$$deploy_dir/$(BINARY)"; \
	fi; \
	for mode in runner watcher; do \
		echo "deploy: starting $$mode"; \
		if [ "$(DRY_RUN)" = "1" ]; then \
			echo "[dry-run] (cd $$deploy_dir && nohup ./$(BINARY) $$mode >> $$mode.log 2>&1 &)"; \
		else \
			(cd "$$deploy_dir" && nohup ./$(BINARY) $$mode >> "$$mode.log" 2>&1 &); \
		fi; \
	done
