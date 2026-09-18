SHELL := /bin/bash
.DEFAULT_GOAL := help

# CLOUD selects the Terraform root module to deploy into. Each cloud is a
# standalone root under terraform/<cloud>/ that emits the same normalized output
# contract (see terraform/README.md), so the K8s/platform steps stay cloud-blind.
CLOUD ?= aws
TF_DIR := terraform/$(CLOUD)
K8S_DIR := k8s
NAMESPACE := darkplum-inference
DEV_CLUSTER := darkplum-dev
# Host port the local dev stack is published on. Override when 8080 is taken:
#   make dev-up DEV_PORT=5174
DEV_PORT ?= 8080
# DEV_AWS=1 mounts your AWS credential files into the dev gateway and enables
# Bedrock (uses DEV_AWS_PROFILE). Off by default: no credentials leave the host.
DEV_AWS ?=
DEV_AWS_PROFILE ?= default
DEV_OVERLAY := $(if $(DEV_AWS),dev-aws,dev)

PROD_TARGETS := verify-bedrock-iam deploy deploy-infra deploy-k8s deploy-platform build-images push-images configure-sso test status destroy deploy-model remove-model list-models
ifneq ($(filter $(PROD_TARGETS),$(MAKECMDGOALS)),)
# Read from the normalized output contract — never reconstruct cloud-specific
# values (e.g. the ECR host from `aws sts get-caller-identity`) in the Makefile.
# Use -json|jq, not -raw: `terraform output -raw` prints a multi-line "No outputs
# found" warning to STDOUT (exit 0) before apply, which -raw would capture as the
# value. -json yields "{}" so these stay empty and the _require-outputs guard fires.
PROVIDER_REGION := $(shell cd $(TF_DIR) && terraform output -json 2>/dev/null | jq -r '.provider_region.value // empty')
CONTAINER_REGISTRY := $(shell cd $(TF_DIR) && terraform output -json 2>/dev/null | jq -r '.container_registry.value // empty')
INFERENCE_REGION := $(shell cd $(TF_DIR) && terraform output -json 2>/dev/null | jq -r '.inference_region.value // empty')
BEDROCK_MODEL_FILTER := $(shell cd $(TF_DIR) && terraform output -json 2>/dev/null | jq -r '.bedrock_model_filter.value // empty')
endif

export DOCKER_CLI_HINTS = false
export DOCKER_CLI_HOOKS = 0

.PHONY: help check-prereqs _require-outputs _deploy-vllm deploy deploy-infra deploy-k8s deploy-platform build-images push-images configure-sso test status destroy \
       deploy-model remove-model list-models verify-bedrock-iam expose-web unexpose-web set-hf-token \
       dev-up dev-down dev-build-gateway dev-build-api dev-build-web dev-status dev-logs dev-test

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

check-prereqs: ## Validate required tools are installed
	@echo "Checking prerequisites for CLOUD=$(CLOUD)..."
	@test -d $(TF_DIR) || { echo "ERROR: no Terraform root at $(TF_DIR). CLOUD=$(CLOUD) is not available — aws is the only installable cloud today."; exit 1; }
	@command -v terraform >/dev/null 2>&1 || { echo "ERROR: terraform not found. Install: https://developer.hashicorp.com/terraform/install"; exit 1; }
	@command -v kubectl >/dev/null 2>&1 || { echo "ERROR: kubectl not found. Install: https://kubernetes.io/docs/tasks/tools/"; exit 1; }
	@command -v jq >/dev/null 2>&1 || { echo "ERROR: jq not found. Install: https://jqlang.github.io/jq/download/"; exit 1; }
	@if [ "$(CLOUD)" = "aws" ]; then \
		command -v aws >/dev/null 2>&1 || { echo "ERROR: aws CLI not found. Install: https://aws.amazon.com/cli/"; exit 1; }; \
		aws sts get-caller-identity >/dev/null 2>&1 || { echo "ERROR: AWS credentials not configured. Run: aws configure"; exit 1; }; \
	fi
	@echo "All prerequisites satisfied."

# Fail loudly if the normalized Terraform outputs aren't available yet (infra not
# applied), instead of tagging/pushing images to an empty registry host.
_require-outputs:
	@test -n "$(CONTAINER_REGISTRY)" || { echo "ERROR: no container_registry output from $(TF_DIR). Run 'make deploy-infra' first."; exit 1; }

deploy-infra: check-prereqs ## Provision cloud infrastructure with Terraform (CLOUD=aws|gcp)
	@echo "Initializing Terraform..."
	@cd $(TF_DIR) && terraform init -input=false
	@echo "Applying Terraform (this takes 15-20 minutes)..."
	@cd $(TF_DIR) && terraform apply -auto-approve -input=false
	@echo "Updating kubeconfig..."
	@eval "$$(cd $(TF_DIR) && terraform output -raw kube_config_command)"
	@echo "Infrastructure provisioned."

deploy-k8s: ## Deploy Kubernetes workloads (self-hosted vLLM inference; skipped when gpu_desired_size = 0)
	@echo "Applying namespaces..."
	@kubectl apply -f $(K8S_DIR)/base/namespaces/darkplum-inference.yaml
	@kubectl apply -f $(K8S_DIR)/base/network-policies/vllm-ingress.yaml
	@kubectl apply -f $(K8S_DIR)/base/network-policies/inference-egress.yaml
	@# gpu_desired_size = 0 is the cheap, Bedrock-only install: no GPU node will
	@# ever register, so waiting for one would fail the whole deploy before the
	@# platform is installed. Self-hosted models can be added later by raising
	@# gpu_desired_size and re-running `make deploy`.
	@GPUS=$$(cd $(TF_DIR) && terraform output -json | jq -r '.gpu_desired_size.value // 1'); \
	if [ "$$GPUS" = "0" ]; then \
		echo "gpu_desired_size = 0: skipping self-hosted vLLM. Cloud-managed (Bedrock) models only."; \
	else \
		$(MAKE) --no-print-directory _deploy-vllm CLOUD=$(CLOUD); \
	fi

_deploy-vllm:
	@echo "NOTE: NVIDIA device plugin is a cluster prerequisite."
	@echo "Ensure it is installed on GPU nodes before proceeding."
	@echo "Waiting for GPU node to be ready..."
	@echo "(this may take 5-10 minutes for the node to register and GPU driver to initialize)"
	@for i in $$(seq 1 60); do \
		GPU_COUNT=$$(kubectl get nodes -l darkplum.ai/node-type=gpu-inference \
			-o jsonpath='{.items[0].status.allocatable.nvidia\.com/gpu}' 2>/dev/null); \
		if [ "$$GPU_COUNT" = "1" ]; then \
			echo "GPU node ready with $$GPU_COUNT GPU(s) allocatable."; \
			break; \
		fi; \
		if [ $$i -eq 60 ]; then \
			echo "ERROR: Timed out waiting for GPU node. Check: kubectl get nodes -l darkplum.ai/node-type=gpu-inference"; \
			exit 1; \
		fi; \
		echo "  Waiting for GPU node... ($$i/60)"; \
		sleep 10; \
	done
	@echo "Deploying vLLM service account..."
	@kubectl apply -f $(K8S_DIR)/overlays/prod/resources/vllm-service-account.yaml
	@echo "Configuring model in deployment..."
	@MODEL_NAME=$$(cd $(TF_DIR) && terraform output -raw model_name) && \
		SERVED_NAME=$$(cd $(TF_DIR) && terraform output -raw served_model_name) && \
		MODEL_REV=$$(cd $(TF_DIR) && terraform output -json | jq -r '.model_revision.value // "main"') && \
		sed -e "s|PLACEHOLDER_MODEL_NAME|$$MODEL_NAME|g" \
			-e "s|PLACEHOLDER_MODEL_REVISION|$$MODEL_REV|g" \
			-e "s|PLACEHOLDER_SERVED_MODEL_NAME|$$SERVED_NAME|g" \
			$(K8S_DIR)/overlays/prod/resources/vllm-deployment.yaml | kubectl apply -f -
	@kubectl apply -f $(K8S_DIR)/overlays/prod/resources/vllm-service.yaml
	@kubectl apply -f $(K8S_DIR)/base/network-policies/vllm-ingress.yaml
	@kubectl apply -f $(K8S_DIR)/base/network-policies/inference-egress.yaml
	@echo "vLLM deployment created. Model download + loading takes 15-25 minutes."
	@echo "Monitor progress: kubectl logs -f -n $(NAMESPACE) deploy/vllm-inference -c model-download"

deploy: deploy-infra deploy-k8s deploy-platform ## Full deploy: infrastructure + workloads + platform
	@echo ""
	@echo "========================================="
	@echo "  Darkplum deployment complete."
	@echo "========================================="
	@echo ""
	@GPUS=$$($(_GPU_COUNT)); \
	if [ "$$GPUS" = "0" ]; then \
		echo "Bedrock-only install (gpu_desired_size = 0). Next:"; \
		echo "  make verify-bedrock-iam    prove the gateway role reaches only the configured Bedrock models"; \
		echo "  make expose-web            publish the web UI (add CERT_ARN=<ACM cert> for HTTPS)"; \
		echo "  then open the printed address, create the first admin, and enable a model under Admin > Models."; \
		echo ""; \
		echo "Add self-hosted open-source models later: set gpu_desired_size = 1 and run 'make deploy' again."; \
	else \
		echo "Wait for vLLM to finish loading (15-25 minutes), then:"; \
		echo "  make test                  chat once through the self-hosted model"; \
		echo "  make verify-bedrock-iam    prove the gateway role reaches only the configured Bedrock models"; \
		echo "  make expose-web            publish the web UI (add CERT_ARN=<ACM cert> for HTTPS)"; \
		echo ""; \
		echo "Monitor model loading:"; \
		echo "  kubectl logs -f -n $(NAMESPACE) deploy/vllm-inference -c model-download"; \
		echo "  kubectl logs -f -n $(NAMESPACE) deploy/vllm-inference -c vllm"; \
	fi
	@echo ""
	@echo "Check readiness:  make status"

cli: ## Build the darkplum operator CLI (bin/darkplum)
	@echo "Building darkplum CLI..."
	@cd services/cli && go build -o ../../bin/darkplum ./cmd
	@echo "Built bin/darkplum — operator commands: doctor / admin / sso / status"

build-images: _require-outputs ## Build Docker images for gateway, api, and web
	@echo "Building gateway image..."
	@docker build -t darkplum-gateway:latest -t $(CONTAINER_REGISTRY)/darkplum-gateway:latest services/gateway/
	@echo "Building api image..."
	@docker build -t darkplum-api:latest -t $(CONTAINER_REGISTRY)/darkplum-api:latest services/api/
	@echo "Building web image..."
	@docker build -t darkplum-web:latest -t $(CONTAINER_REGISTRY)/darkplum-web:latest services/web/
	@echo "Images built and tagged for ECR."

push-images: _require-outputs ## Push Docker images to the container registry (AWS ECR today)
	@echo "Ensuring ECR repositories exist..."
	@aws ecr describe-repositories --repository-names darkplum-gateway --region $(PROVIDER_REGION) >/dev/null 2>&1 || \
		aws ecr create-repository --repository-name darkplum-gateway --region $(PROVIDER_REGION) >/dev/null
	@aws ecr describe-repositories --repository-names darkplum-api --region $(PROVIDER_REGION) >/dev/null 2>&1 || \
		aws ecr create-repository --repository-name darkplum-api --region $(PROVIDER_REGION) >/dev/null
	@aws ecr describe-repositories --repository-names darkplum-web --region $(PROVIDER_REGION) >/dev/null 2>&1 || \
		aws ecr create-repository --repository-name darkplum-web --region $(PROVIDER_REGION) >/dev/null
	@echo "Logging into ECR..."
	@aws ecr get-login-password --region $(PROVIDER_REGION) | docker login --username AWS --password-stdin $(CONTAINER_REGISTRY)
	@echo "Pushing gateway image..."
	@docker push $(CONTAINER_REGISTRY)/darkplum-gateway:latest
	@echo "Pushing api image..."
	@docker push $(CONTAINER_REGISTRY)/darkplum-api:latest
	@echo "Pushing web image..."
	@docker push $(CONTAINER_REGISTRY)/darkplum-web:latest
	@echo "Images pushed to ECR."

deploy-platform: build-images push-images ## Build, push, and deploy gateway + web into darkplum-system
	@echo "Applying darkplum-system namespace..."
	@kubectl apply -f $(K8S_DIR)/base/namespaces/darkplum-system.yaml
	@echo "Deploying gateway service account..."
	@kubectl apply -f $(K8S_DIR)/base/gateway/service-account.yaml
	@echo "Creating database credentials secret from Terraform output..."
	@DATABASE_URL=$$(cd $(TF_DIR) && terraform output -raw database_url) && \
		kubectl create secret generic postgres-credentials \
			--namespace=darkplum-system \
			--from-literal=DATABASE_URL="$$DATABASE_URL" \
			--dry-run=client -o yaml | kubectl apply -f -
	@echo "Creating OIDC credentials secret..."
	@EXISTING_SESSION_SIGNING_KEY=$$(kubectl get secret darkplum-auth -n darkplum-system \
		-o jsonpath='{.data.SESSION_SIGNING_KEY}' 2>/dev/null | base64 -d 2>/dev/null); \
		SESSION_SIGNING_KEY=$${SESSION_SIGNING_KEY:-$${EXISTING_SESSION_SIGNING_KEY:-$$(openssl rand -base64 48)}}; \
		EXISTING_SERVICE_KEY_SECRET=$$(kubectl get secret darkplum-auth -n darkplum-system \
			-o jsonpath='{.data.SERVICE_KEY_SECRET}' 2>/dev/null | base64 -d 2>/dev/null); \
		SERVICE_KEY_SECRET=$${SERVICE_KEY_SECRET:-$${EXISTING_SERVICE_KEY_SECRET:-$$(openssl rand -base64 48)}}; \
		OIDC_ISSUER=$${OIDC_ISSUER:-unconfigured}; \
		OIDC_CLIENT_ID=$${OIDC_CLIENT_ID:-unconfigured}; \
		OIDC_CLIENT_SECRET=$${OIDC_CLIENT_SECRET:-unconfigured}; \
		OIDC_REDIRECT_URL=$${OIDC_REDIRECT_URL:-unconfigured}; \
		if [ "$$OIDC_ISSUER" = "unconfigured" ]; then \
			echo "NOTE: OIDC not configured. Local username/password auth will be used."; \
			echo "Set OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, OIDC_REDIRECT_URL to enable SSO."; \
		fi; \
		kubectl create secret generic darkplum-auth \
			--namespace=darkplum-system \
			--from-literal=OIDC_ISSUER="$$OIDC_ISSUER" \
			--from-literal=OIDC_CLIENT_ID="$$OIDC_CLIENT_ID" \
			--from-literal=OIDC_CLIENT_SECRET="$$OIDC_CLIENT_SECRET" \
			--from-literal=OIDC_REDIRECT_URL="$$OIDC_REDIRECT_URL" \
			--from-literal=SESSION_SIGNING_KEY="$$SESSION_SIGNING_KEY" \
			--from-literal=SERVICE_KEY_SECRET="$$SERVICE_KEY_SECRET" \
			--dry-run=client -o yaml | kubectl apply -f -
	@# The api owns the schema: it must migrate before the gateway rolls, because
	@# the gateway fails closed (503 policy_unavailable) until it can load routing
	@# policy from tables the api creates.
	@echo "Deploying api (runs database migrations)..."
	@sed "s|darkplum-api:latest|$(CONTAINER_REGISTRY)/darkplum-api:latest|" \
		$(K8S_DIR)/base/api/deployment.yaml | kubectl apply -f -
	@kubectl apply -f $(K8S_DIR)/base/api/service.yaml
	@kubectl rollout status deployment/darkplum-api -n darkplum-system --timeout=300s
	@echo "Deploying gateway..."
	@# Bedrock region + catalog filter come from Terraform so the catalog matches
	@# the IAM allowlist. VLLM_UPSTREAMS is owned by deploy-model/remove-model:
	@# carry the live value over so re-running this target does not drop models.
	@CURRENT_UPSTREAMS=$$(kubectl get deploy darkplum-gateway -n darkplum-system \
		-o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="VLLM_UPSTREAMS")].value}' 2>/dev/null); \
	sed -e "s|darkplum-gateway:latest|$(CONTAINER_REGISTRY)/darkplum-gateway:latest|" \
		$(K8S_DIR)/base/gateway/deployment.yaml | \
	BEDROCK_REGION="$(INFERENCE_REGION)" BEDROCK_MODEL_FILTER="$(BEDROCK_MODEL_FILTER)" VLLM_UPSTREAMS="$$CURRENT_UPSTREAMS" \
	awk ' \
		/- name: BEDROCK_REGION$$/       { print; getline; if (ENVIRON["BEDROCK_REGION"] != "") sub(/value: .*/, "value: \"" ENVIRON["BEDROCK_REGION"] "\""); print; next } \
		/- name: BEDROCK_MODEL_FILTER$$/ { print; getline; sub(/value: .*/, "value: \"" ENVIRON["BEDROCK_MODEL_FILTER"] "\""); print; next } \
		/- name: VLLM_UPSTREAMS$$/       { print; getline; sub(/value: .*/, "value: \"" ENVIRON["VLLM_UPSTREAMS"] "\""); print; next } \
		{ print }' | kubectl apply -f -
	@kubectl apply -f $(K8S_DIR)/base/gateway/service.yaml
	@kubectl rollout status deployment/darkplum-gateway -n darkplum-system --timeout=300s
	@echo "Deploying web..."
	@sed "s|darkplum-web:latest|$(CONTAINER_REGISTRY)/darkplum-web:latest|" \
		$(K8S_DIR)/base/web/deployment.yaml | kubectl apply -f -
	@kubectl apply -f $(K8S_DIR)/base/web/service.yaml
	@echo "Applying network policies..."
	@kubectl apply -f $(K8S_DIR)/base/network-policies/vllm-ingress.yaml
	@kubectl apply -f $(K8S_DIR)/base/network-policies/gateway-egress.yaml
	@echo ""
	@echo "Platform components deployed to darkplum-system."
	@echo "Check status: kubectl get pods -n darkplum-system"
	@echo "Web endpoint:  kubectl get svc darkplum-web -n darkplum-system"

configure-sso: ## Configure OIDC/SSO authentication (interactive)
	@echo "=== Darkplum SSO Configuration ==="
	@echo ""
	@if [ -z "$$OIDC_ISSUER" ]; then \
		read -p "OIDC Issuer URL (e.g. https://accounts.google.com): " OIDC_ISSUER; \
	fi; \
	if [ -z "$$OIDC_CLIENT_ID" ]; then \
		read -p "OIDC Client ID: " OIDC_CLIENT_ID; \
	fi; \
	if [ -z "$$OIDC_CLIENT_SECRET" ]; then \
		read -sp "OIDC Client Secret: " OIDC_CLIENT_SECRET; echo; \
	fi; \
	WEB_URL=$$(kubectl get svc darkplum-web -n darkplum-system -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null); \
	if [ -z "$$WEB_URL" ]; then \
		read -p "OIDC Redirect URL (e.g. https://darkplum.company.com/auth/callback): " OIDC_REDIRECT_URL; \
	else \
		WEB_SCHEME=$$(kubectl get svc darkplum-web -n darkplum-system -o jsonpath='{.spec.ports[0].port}' 2>/dev/null | grep -q '^443$$' && echo https || echo http); \
		DEFAULT_REDIRECT="$$WEB_SCHEME://$$WEB_URL/auth/callback"; \
		read -p "OIDC Redirect URL [$$DEFAULT_REDIRECT]: " OIDC_REDIRECT_URL; \
		OIDC_REDIRECT_URL=$${OIDC_REDIRECT_URL:-$$DEFAULT_REDIRECT}; \
	fi; \
	EXISTING_SESSION_SIGNING_KEY=$$(kubectl get secret darkplum-auth -n darkplum-system \
		-o jsonpath='{.data.SESSION_SIGNING_KEY}' 2>/dev/null | base64 -d 2>/dev/null); \
	SESSION_SIGNING_KEY=$${EXISTING_SESSION_SIGNING_KEY:-$$(openssl rand -base64 48)}; \
	EXISTING_SERVICE_KEY_SECRET=$$(kubectl get secret darkplum-auth -n darkplum-system \
		-o jsonpath='{.data.SERVICE_KEY_SECRET}' 2>/dev/null | base64 -d 2>/dev/null); \
	SERVICE_KEY_SECRET=$${EXISTING_SERVICE_KEY_SECRET:-$$(openssl rand -base64 48)}; \
	echo ""; \
	echo "Updating darkplum-auth secret..."; \
	kubectl create secret generic darkplum-auth \
		--namespace=darkplum-system \
		--from-literal=OIDC_ISSUER="$$OIDC_ISSUER" \
		--from-literal=OIDC_CLIENT_ID="$$OIDC_CLIENT_ID" \
		--from-literal=OIDC_CLIENT_SECRET="$$OIDC_CLIENT_SECRET" \
		--from-literal=OIDC_REDIRECT_URL="$$OIDC_REDIRECT_URL" \
		--from-literal=SESSION_SIGNING_KEY="$$SESSION_SIGNING_KEY" \
		--from-literal=SERVICE_KEY_SECRET="$$SERVICE_KEY_SECRET" \
		--dry-run=client -o yaml | kubectl apply -f -; \
	echo "Restarting darkplum-api to pick up new config..."; \
	kubectl rollout restart deployment/darkplum-api -n darkplum-system; \
	kubectl rollout status deployment/darkplum-api -n darkplum-system --timeout=120s; \
	echo ""; \
	echo "SSO configured. Users will now see 'Sign in with SSO' at $$OIDC_REDIRECT_URL"; \
	echo ""; \
	echo "IMPORTANT: Add this redirect URL to your identity provider:"; \
	echo "  $$OIDC_REDIRECT_URL"

test: ## Send a test chat completion request to the self-hosted vLLM model
	@GPUS=$$($(_GPU_COUNT)); [ "$$GPUS" != "0" ] || { \
		echo "This is a Bedrock-only install (gpu_desired_size = 0): there is no self-hosted vLLM to test."; \
		echo "Chat through the web UI instead (make expose-web), or check the gateway's view of Bedrock:"; \
		echo "  kubectl port-forward -n darkplum-system svc/darkplum-gateway 8082:8080 &  curl -s localhost:8082/health | jq .routing"; \
		exit 0; }
	@echo "Port-forwarding to vLLM service..."
	@kubectl port-forward -n $(NAMESPACE) svc/vllm-inference 8000:8000 &>/dev/null &
	@PF_PID=$$!; \
	sleep 8; \
	SERVED_NAME=$$(cd $(TF_DIR) && terraform output -raw served_model_name); \
	echo "Sending test request to model: $$SERVED_NAME"; \
	RESPONSE=$$(curl -s -w "\n%{http_code}" http://localhost:8000/v1/chat/completions \
		-H "Content-Type: application/json" \
		-d "{\"model\": \"$$SERVED_NAME\", \"messages\": [{\"role\": \"user\", \"content\": \"Say hello in one sentence.\"}], \"max_tokens\": 50}"); \
	HTTP_CODE=$$(echo "$$RESPONSE" | tail -1); \
	BODY=$$(echo "$$RESPONSE" | sed '$$d'); \
	kill $$PF_PID 2>/dev/null; \
	echo ""; \
	if [ "$$HTTP_CODE" = "200" ]; then \
		CONTENT=$$(echo "$$BODY" | jq -r '.choices[0].message.content // empty'); \
		if [ -n "$$CONTENT" ]; then \
			echo "SUCCESS: HTTP $$HTTP_CODE"; \
			echo "Response: $$CONTENT"; \
		else \
			echo "FAIL: HTTP 200 but empty completion."; \
			echo "Body: $$BODY"; \
			exit 1; \
		fi; \
	else \
		echo "FAIL: HTTP $$HTTP_CODE"; \
		echo "Body: $$BODY"; \
		echo ""; \
		echo "vLLM may still be loading. Check: make status"; \
		exit 1; \
	fi

status: ## Show cluster and workload status
	@echo "=== Cluster ==="
	@kubectl cluster-info 2>/dev/null | head -1 || echo "kubeconfig not set. Run: make deploy-infra"
	@echo ""
	@echo "=== Platform (darkplum-system) ==="
	@kubectl get pods -n darkplum-system -o wide 2>/dev/null || echo "Platform not deployed. Run: make deploy"
	@echo ""
	@echo "=== Web UI ==="
	@HOST=$$(kubectl get svc darkplum-web -n darkplum-system -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null); \
	PORT=$$(kubectl get svc darkplum-web -n darkplum-system -o jsonpath='{.spec.ports[0].port}' 2>/dev/null); \
	if [ -n "$$HOST" ]; then echo "$$([ "$$PORT" = "443" ] && echo https || echo http)://$$HOST"; \
	else echo "cluster-internal (make expose-web to publish; or kubectl port-forward -n darkplum-system svc/darkplum-web 8081:80)"; fi
	@echo ""
	@GPUS=$$($(_GPU_COUNT)); \
	if [ "$$GPUS" = "0" ]; then \
		echo "=== Self-hosted models ==="; \
		echo "none — Bedrock-only install (gpu_desired_size = 0)"; \
	else \
		echo "=== Nodes ==="; \
		kubectl get nodes -L darkplum.ai/node-type 2>/dev/null || true; \
		echo ""; \
		echo "=== GPU Allocatable ==="; \
		kubectl get nodes -l darkplum.ai/node-type=gpu-inference \
			-o custom-columns="NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu,STATUS:.status.conditions[-1].type" \
			2>/dev/null || echo "No GPU nodes found."; \
		echo ""; \
		echo "=== Self-hosted model pods ==="; \
		kubectl get pods -n $(NAMESPACE) -o wide 2>/dev/null || true; \
		echo ""; \
		echo "=== vLLM Logs (last 5 lines) ==="; \
		kubectl logs -n $(NAMESPACE) deploy/vllm-inference -c vllm --tail=5 2>/dev/null || \
			kubectl logs -n $(NAMESPACE) deploy/vllm-inference -c model-download --tail=5 2>/dev/null || \
			echo "No vLLM pods running yet."; \
	fi

destroy: ## Tear down all resources
	@echo "Deleting Kubernetes resources..."
	@kubectl delete -f $(K8S_DIR)/base/gateway/service.yaml --ignore-not-found 2>/dev/null || true
	@kubectl delete -f $(K8S_DIR)/base/gateway/deployment.yaml --ignore-not-found 2>/dev/null || true
	@kubectl delete -f $(K8S_DIR)/base/gateway/service-account.yaml --ignore-not-found 2>/dev/null || true
	@kubectl delete -f $(K8S_DIR)/base/api/deployment.yaml --ignore-not-found 2>/dev/null || true
	@kubectl delete -f $(K8S_DIR)/base/web/service.yaml --ignore-not-found 2>/dev/null || true
	@kubectl delete -f $(K8S_DIR)/base/web/deployment.yaml --ignore-not-found 2>/dev/null || true
	@kubectl delete -f $(K8S_DIR)/base/network-policies/vllm-ingress.yaml --ignore-not-found 2>/dev/null || true
	@kubectl delete -f $(K8S_DIR)/base/network-policies/gateway-egress.yaml --ignore-not-found 2>/dev/null || true
	@kubectl delete -f $(K8S_DIR)/base/network-policies/inference-egress.yaml --ignore-not-found 2>/dev/null || true
	@kubectl delete deploy,svc -n $(NAMESPACE) -l darkplum.ai/model-instance --ignore-not-found 2>/dev/null || true
	@kubectl delete -f $(K8S_DIR)/overlays/prod/resources/vllm-service.yaml --ignore-not-found 2>/dev/null || true
	@kubectl delete -f $(K8S_DIR)/overlays/prod/resources/vllm-deployment.yaml --ignore-not-found 2>/dev/null || true
	@kubectl delete -f $(K8S_DIR)/overlays/prod/resources/vllm-service-account.yaml --ignore-not-found 2>/dev/null || true
	@echo "Deleting namespaces (timeout 120s each)..."
	@kubectl delete namespace darkplum-system --timeout=120s 2>/dev/null || \
		kubectl delete namespace darkplum-system --force --grace-period=0 2>/dev/null || true
	@kubectl delete namespace $(NAMESPACE) --timeout=120s 2>/dev/null || \
		kubectl delete namespace $(NAMESPACE) --force --grace-period=0 2>/dev/null || true
	@echo "Destroying infrastructure (this takes 10-15 minutes)..."
	@cd $(TF_DIR) && terraform destroy -auto-approve -input=false
	@echo "All resources destroyed."

verify-bedrock-iam: ## Prove the gateway's IAM role allows only the configured Bedrock models (Layer 1 check)
	@ROLE=$$(cd $(TF_DIR) && terraform output -json | jq -r '.gateway_role_arn.value // empty'); \
	REGION=$$(cd $(TF_DIR) && terraform output -json | jq -r '.inference_region.value // empty'); \
	ALLOWED=$$(cd $(TF_DIR) && terraform output -json | jq -r '.bedrock_invoke_arns.value[]? | select(test("inference-profile/"))' | head -1); \
	[ -n "$$ROLE" ] || { echo "ERROR: no gateway_role_arn output. Run 'make deploy-infra' first."; exit 1; }; \
	ACCOUNT=$$(echo "$$ROLE" | cut -d: -f5); \
	sim() { aws iam simulate-principal-policy --policy-source-arn "$$ROLE" --action-names bedrock:InvokeModel \
		--resource-arns "$$1" --query 'EvaluationResults[0].EvalDecision' --output text; }; \
	FAIL=0; \
	check() { GOT=$$(sim "$$2"); if [ "$$GOT" = "$$1" ]; then echo "  ok    $$1  $$2"; else echo "  FAIL  expected $$1, got $$GOT  $$2"; FAIL=1; fi; }; \
	echo "Simulating bedrock:InvokeModel as $$ROLE"; \
	if [ -n "$$ALLOWED" ]; then check allowed "$$(echo "$$ALLOWED" | sed 's/\*$$/example-v1:0/')"; else echo "  (no Bedrock models are allowed; checking denials only)"; fi; \
	check implicitDeny "arn:aws:bedrock:$$REGION:$$ACCOUNT:inference-profile/global.anthropic.claude-example-v1:0"; \
	check implicitDeny "arn:aws:bedrock:$$REGION::foundation-model/darkplum.not-an-allowed-model-v1:0"; \
	check implicitDeny "arn:aws:bedrock:ap-southeast-7::foundation-model/anthropic.claude-example-v1:0"; \
	[ $$FAIL -eq 0 ] && echo "Layer 1 allowlist behaves as configured." || { echo "Layer 1 allowlist does NOT behave as configured."; exit 1; }

# =============================================================================
# Exposing the web UI (production)
# =============================================================================

SCHEME ?= internal

expose-web: ## Publish the web UI on an AWS NLB: make expose-web [SCHEME=internal|internet-facing] [CERT_ARN=<ACM cert arn> for HTTPS]
	@[ "$(SCHEME)" = "internal" ] || [ "$(SCHEME)" = "internet-facing" ] || { echo "ERROR: SCHEME must be internal or internet-facing."; exit 1; }
	@kubectl get svc darkplum-web -n darkplum-system >/dev/null 2>&1 || { echo "ERROR: darkplum-web is not deployed. Run 'make deploy' first."; exit 1; }
	@kubectl get nodes -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null | grep -q '^aws://' || { echo "ERROR: this is not an EKS cluster (the local dev stack is reached on http://localhost:$(DEV_PORT)). expose-web provisions an AWS load balancer."; exit 1; }
	@if [ -n "$(CERT_ARN)" ]; then \
		echo "Exposing web UI over HTTPS ($(SCHEME) NLB, TLS terminated with $(CERT_ARN))..."; \
		PORTS='[{"name":"https","port":443,"targetPort":80,"protocol":"TCP"}]'; \
		ANN='{"service.beta.kubernetes.io/aws-load-balancer-type":"nlb","service.beta.kubernetes.io/aws-load-balancer-scheme":"$(SCHEME)","service.beta.kubernetes.io/aws-load-balancer-internal":"$(if $(filter internal,$(SCHEME)),true,false)","service.beta.kubernetes.io/aws-load-balancer-ssl-cert":"$(CERT_ARN)","service.beta.kubernetes.io/aws-load-balancer-ssl-ports":"443","service.beta.kubernetes.io/aws-load-balancer-backend-protocol":"http"}'; \
		SECURE=true; \
	else \
		echo "Exposing web UI over plain HTTP ($(SCHEME) NLB)."; \
		echo "WARNING: without CERT_ARN sessions travel unencrypted and session cookies lose the Secure flag."; \
		echo "         Use this only on a private network, and only until you have an ACM certificate."; \
		[ "$(SCHEME)" = "internal" ] || { echo "ERROR: refusing to publish plain HTTP on an internet-facing load balancer. Pass CERT_ARN."; exit 1; }; \
		PORTS='[{"name":"http","port":80,"targetPort":80,"protocol":"TCP"}]'; \
		ANN='{"service.beta.kubernetes.io/aws-load-balancer-type":"nlb","service.beta.kubernetes.io/aws-load-balancer-scheme":"internal","service.beta.kubernetes.io/aws-load-balancer-internal":"true","service.beta.kubernetes.io/aws-load-balancer-ssl-cert":null,"service.beta.kubernetes.io/aws-load-balancer-ssl-ports":null,"service.beta.kubernetes.io/aws-load-balancer-backend-protocol":null}'; \
		SECURE=false; \
	fi; \
	kubectl patch svc darkplum-web -n darkplum-system --type=merge \
		-p "{\"metadata\":{\"annotations\":$$ANN},\"spec\":{\"type\":\"LoadBalancer\",\"ports\":$$PORTS}}" >/dev/null; \
	echo "Waiting for the load balancer hostname (EKS provisions the NLB; usually 1-3 minutes)..."; \
	for i in $$(seq 1 30); do \
		HOST=$$(kubectl get svc darkplum-web -n darkplum-system -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null); \
		[ -n "$$HOST" ] && break; sleep 10; \
	done; \
	if [ -z "$$HOST" ]; then \
		echo "ERROR: no load balancer was provisioned. Reverting to cluster-internal so nothing is left half-configured."; \
		echo "       Check: kubectl describe svc darkplum-web -n darkplum-system   (events show why EKS could not create the NLB)"; \
		$(MAKE) --no-print-directory unexpose-web; exit 1; \
	fi; \
	CURRENT=$$(kubectl get secret darkplum-auth -n darkplum-system -o jsonpath='{.data.SECURE_COOKIES}' 2>/dev/null | base64 -d 2>/dev/null); \
	if [ "$$CURRENT" != "$$SECURE" ]; then \
		kubectl patch secret darkplum-auth -n darkplum-system --type=merge \
			-p "{\"stringData\":{\"SECURE_COOKIES\":\"$$SECURE\"}}" >/dev/null; \
		kubectl rollout restart deployment/darkplum-api -n darkplum-system >/dev/null; \
		kubectl rollout status deployment/darkplum-api -n darkplum-system --timeout=120s; \
	fi; \
	SCHEME_URL=$$([ "$$SECURE" = "true" ] && echo https || echo http); \
	echo ""; \
	echo "Web UI: $$SCHEME_URL://$$HOST"; \
	[ "$$SECURE" != "true" ] || echo "Point your DNS name at this hostname (CNAME) — the certificate must match the name users type."; \
	echo "Then: make configure-sso   (to enable SSO; uses this address for the redirect URL)"

unexpose-web: ## Revert the web UI to cluster-internal (port-forward only)
	@kubectl patch svc darkplum-web -n darkplum-system --type=merge \
		-p '{"metadata":{"annotations":{"service.beta.kubernetes.io/aws-load-balancer-type":null,"service.beta.kubernetes.io/aws-load-balancer-scheme":null,"service.beta.kubernetes.io/aws-load-balancer-internal":null,"service.beta.kubernetes.io/aws-load-balancer-ssl-cert":null,"service.beta.kubernetes.io/aws-load-balancer-ssl-ports":null,"service.beta.kubernetes.io/aws-load-balancer-backend-protocol":null}},"spec":{"type":"ClusterIP","ports":[{"name":"http","port":80,"targetPort":80,"protocol":"TCP"}]}}' >/dev/null
	@CURRENT=$$(kubectl get secret darkplum-auth -n darkplum-system -o jsonpath='{.data.SECURE_COOKIES}' 2>/dev/null | base64 -d 2>/dev/null); \
	if [ "$$CURRENT" != "true" ]; then \
		kubectl patch secret darkplum-auth -n darkplum-system --type=merge -p '{"stringData":{"SECURE_COOKIES":"true"}}' >/dev/null; \
		kubectl rollout restart deployment/darkplum-api -n darkplum-system >/dev/null; \
	fi
	@echo "Web UI is cluster-internal again: kubectl port-forward -n darkplum-system svc/darkplum-web 8081:80"

# =============================================================================
# Additional self-hosted open-source models (one vLLM deployment + GPU each)
# =============================================================================

MAX_LEN ?= 8192
# Number of GPU nodes the current Terraform state asked for ("" before apply).
_GPU_COUNT = cd $(TF_DIR) && terraform output -json 2>/dev/null | jq -r '.gpu_desired_size.value // empty'
# Hugging Face revision for deploy-model. Pin a commit SHA for anything that matters.
REVISION ?= main
_GW_UPSTREAMS = kubectl get deploy darkplum-gateway -n darkplum-system \
	-o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="VLLM_UPSTREAMS")].value}' 2>/dev/null

deploy-model: ## Add a self-hosted model: make deploy-model NAME=qwen MODEL=Qwen/Qwen2.5-7B-Instruct [REVISION=<commit-sha>] [SERVED=qwen-2.5-7b] [MAX_LEN=8192]
	@[[ "$(NAME)" =~ ^[a-z0-9][a-z0-9-]{0,30}$$ ]] || { echo "ERROR: NAME is required: lowercase letters, digits and dashes (max 31 chars). Got '$(NAME)'."; exit 1; }
	@[ "$(NAME)" != "default" ] && [ "$(NAME)" != "inference" ] || { echo "ERROR: NAME '$(NAME)' is reserved for the default vLLM deployment."; exit 1; }
	@[ -n "$(MODEL)" ] || { echo "ERROR: MODEL is required (Hugging Face model ID, e.g. Qwen/Qwen2.5-7B-Instruct)."; exit 1; }
	@kubectl get deploy darkplum-gateway -n darkplum-system >/dev/null 2>&1 || { echo "ERROR: gateway not deployed. Run 'make deploy' first."; exit 1; }
	@SERVED="$(if $(SERVED),$(SERVED),$(NAME))"; \
	TOTAL=$$(kubectl get nodes -l darkplum.ai/node-type=gpu-inference -o json | jq '[.items[].status.allocatable["nvidia.com/gpu"] // "0" | tonumber] | add // 0'); \
	USED=$$(kubectl get pods -n $(NAMESPACE) -o json | jq '[.items[] | select(.status.phase=="Running" or .status.phase=="Pending") | .spec.containers[].resources.requests["nvidia.com/gpu"] // "0" | tonumber] | add // 0'); \
	EXISTS=$$(kubectl get deploy vllm-$(NAME) -n $(NAMESPACE) -o name 2>/dev/null); \
	if [ -z "$$EXISTS" ] && [ $$((TOTAL - USED)) -lt 1 ]; then \
		echo "WARNING: no free GPU ($$USED of $$TOTAL allocated). vllm-$(NAME) will stay Pending until the GPU"; \
		echo "         node group grows: raise gpu_desired_size in $(TF_DIR)/terraform.tfvars and run 'make deploy-infra'."; \
	fi; \
	[ "$(REVISION)" != "main" ] || echo "NOTE: REVISION=main is a moving target. Pin a commit SHA (REVISION=<sha>) so weights cannot change under you."; \
	echo "Deploying vllm-$(NAME): $(MODEL)@$(REVISION) served as '$$SERVED'..."; \
	sed -e "s|MODEL_INSTANCE|$(NAME)|g" \
		-e "s|PLACEHOLDER_MODEL_NAME|$(MODEL)|g" \
		-e "s|PLACEHOLDER_MODEL_REVISION|$(REVISION)|g" \
		-e "s|PLACEHOLDER_SERVED_MODEL_NAME|$$SERVED|g" \
		-e "s|PLACEHOLDER_MAX_MODEL_LEN|$(MAX_LEN)|g" \
		$(K8S_DIR)/templates/vllm-model.yaml | kubectl apply -f -
	@CURRENT=$$($(_GW_UPSTREAMS)); \
	ENTRY="$(NAME)=http://vllm-$(NAME).$(NAMESPACE).svc.cluster.local:8000"; \
	UPDATED=$$(echo "$$CURRENT" | tr ',' '\n' | grep -v "^$(NAME)=" | grep -v '^$$' | { cat; echo "$$ENTRY"; } | paste -sd, -); \
	if [ "$$UPDATED" != "$$CURRENT" ]; then \
		echo "Registering upstream with the gateway..."; \
		kubectl set env deploy/darkplum-gateway -n darkplum-system VLLM_UPSTREAMS="$$UPDATED" >/dev/null; \
		kubectl rollout status deploy/darkplum-gateway -n darkplum-system --timeout=120s; \
	fi
	@echo ""
	@echo "Model download + load takes 15-25 minutes. Monitor:"
	@echo "  kubectl logs -f -n $(NAMESPACE) deploy/vllm-$(NAME) -c model-download"
	@echo "Once ready, the gateway discovers it within a minute; then enable it under Admin > Models."

set-hf-token: ## Store a Hugging Face token for gated models (reads HF_TOKEN from the environment, never argv)
	@[ -n "$$HF_TOKEN" ] || { echo "ERROR: set HF_TOKEN in the environment first, e.g.  read -s HF_TOKEN && export HF_TOKEN"; exit 1; }
	@kubectl get namespace $(NAMESPACE) >/dev/null 2>&1 || kubectl apply -f $(K8S_DIR)/base/namespaces/darkplum-inference.yaml >/dev/null
	@kubectl create secret generic huggingface-token --namespace=$(NAMESPACE) \
		--from-literal=token="$$HF_TOKEN" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	@echo "Hugging Face token stored in $(NAMESPACE)/huggingface-token. New model downloads use it; restart an existing"
	@echo "deployment to pick it up: kubectl rollout restart -n $(NAMESPACE) deploy/<vllm-deployment>"

remove-model: ## Remove a self-hosted model added with deploy-model: make remove-model NAME=qwen
	@[[ "$(NAME)" =~ ^[a-z0-9][a-z0-9-]{0,30}$$ ]] || { echo "ERROR: NAME is required."; exit 1; }
	@CURRENT=$$($(_GW_UPSTREAMS)); \
	UPDATED=$$(echo "$$CURRENT" | tr ',' '\n' | grep -v "^$(NAME)=" | grep -v '^$$' | paste -sd, -); \
	if [ "$$UPDATED" != "$$CURRENT" ]; then \
		echo "Unregistering upstream from the gateway..."; \
		kubectl set env deploy/darkplum-gateway -n darkplum-system VLLM_UPSTREAMS="$$UPDATED" >/dev/null; \
		kubectl rollout status deploy/darkplum-gateway -n darkplum-system --timeout=120s; \
	fi
	@kubectl delete deploy,svc -n $(NAMESPACE) -l darkplum.ai/model-instance=$(NAME) --ignore-not-found
	@echo "Removed vllm-$(NAME). Aliases pointing at its model now refuse requests until retargeted."

list-models: ## List self-hosted model deployments and the gateway's registered upstreams
	@echo "=== Self-hosted model deployments ==="
	@kubectl get deploy -n $(NAMESPACE) -l app.kubernetes.io/component=inference-backend \
		-o custom-columns='NAME:.metadata.name,READY:.status.readyReplicas,MODEL:.spec.template.spec.initContainers[0].env[0].value' 2>/dev/null || true
	@kubectl get deploy vllm-inference -n $(NAMESPACE) -o name >/dev/null 2>&1 && echo "(vllm-inference is the default deployment from 'make deploy')" || true
	@echo ""
	@echo "=== Gateway VLLM_UPSTREAMS ==="
	@U=$$($(_GW_UPSTREAMS)); [ -n "$$U" ] && echo "$$U" | tr ',' '\n' || echo "(none beyond the default upstream)"

# =============================================================================
# Local Development (kind)
# =============================================================================

dev-up: ## Create local kind cluster and deploy all services [DEV_PORT=8080] [DEV_AWS=1 DEV_AWS_PROFILE=default]
	@command -v kind >/dev/null 2>&1 || { echo "ERROR: kind not found. Install: https://kind.sigs.k8s.io/docs/user/quick-start/#installation"; exit 1; }
	@command -v kubectl >/dev/null 2>&1 || { echo "ERROR: kubectl not found."; exit 1; }
	@command -v docker >/dev/null 2>&1 || { echo "ERROR: docker not found."; exit 1; }
	@if kind get clusters 2>/dev/null | grep -q "^$(DEV_CLUSTER)$$"; then \
		echo "Cluster $(DEV_CLUSTER) already exists. Run 'make dev-down' first to recreate."; \
		exit 1; \
	fi
	@if command -v lsof >/dev/null 2>&1 && lsof -nP -iTCP:$(DEV_PORT) -sTCP:LISTEN >/dev/null 2>&1; then \
		echo "ERROR: port $(DEV_PORT) is already in use on this machine. Pick another: make dev-up DEV_PORT=5174"; \
		exit 1; \
	fi
	@echo "Creating kind cluster (http://localhost:$(DEV_PORT))..."
	@TMPCONFIG=$$(mktemp) && \
		sed "s|hostPort: 8080|hostPort: $(DEV_PORT)|" $(K8S_DIR)/overlays/dev/kind-config.yaml > $$TMPCONFIG && \
		if [ -n "$(DEV_AWS)" ]; then \
			echo "DEV_AWS: mounting AWS credential files (read-only) for profile '$(DEV_AWS_PROFILE)'"; \
			echo "    extraMounts:" >> $$TMPCONFIG; \
			FOUND=0; \
			for f in config credentials sso; do \
				if [ -e "$$HOME/.aws/$$f" ]; then \
					FOUND=1; \
					printf '      - hostPath: %s\n        containerPath: /host-users/.aws/%s\n        readOnly: true\n' "$$HOME/.aws/$$f" "$$f" >> $$TMPCONFIG; \
				fi; \
			done; \
			[ $$FOUND -eq 1 ] || { echo "ERROR: DEV_AWS=1 but no ~/.aws/config, credentials or sso found."; rm -f $$TMPCONFIG; exit 1; }; \
		fi; \
		kind create cluster --name $(DEV_CLUSTER) --config $$TMPCONFIG; RC=$$?; \
		rm -f $$TMPCONFIG; exit $$RC
	@echo "Building gateway image..."
	@docker build -t darkplum-gateway:latest services/gateway/
	@echo "Building api image..."
	@docker build -t darkplum-api:latest services/api/
	@echo "Building web image..."
	@docker build -t darkplum-web:latest services/web/
	@echo "Loading images into kind..."
	@kind load docker-image --name $(DEV_CLUSTER) darkplum-gateway:latest darkplum-api:latest darkplum-web:latest
	@echo "Deploying all services ($(DEV_OVERLAY) overlay)..."
	@kubectl kustomize $(K8S_DIR)/overlays/$(DEV_OVERLAY) | \
		sed -e "s|http://localhost:8080|http://localhost:$(DEV_PORT)|g" \
		    -e "s|PLACEHOLDER_AWS_PROFILE|$(DEV_AWS_PROFILE)|g" | \
		kubectl apply -f -
	@echo "Waiting for services to be ready..."
	@sleep 5
	@kubectl wait --namespace=darkplum-system --for=condition=ready pod -l app.kubernetes.io/name=postgres --timeout=60s
	@kubectl wait --namespace=darkplum-system --for=condition=ready pod -l app.kubernetes.io/name=darkplum-gateway --timeout=90s
	@kubectl wait --namespace=darkplum-system --for=condition=ready pod -l app.kubernetes.io/name=darkplum-api --timeout=90s
	@kubectl wait --namespace=darkplum-system --for=condition=ready pod -l app.kubernetes.io/name=darkplum-web --timeout=60s
	@echo ""
	@echo "========================================="
	@echo "  Local dev cluster ready."
	@echo "  http://localhost:$(DEV_PORT)"
	@echo "========================================="
	@echo ""
	@echo "First run: create an admin account at http://localhost:$(DEV_PORT),"
	@echo "then enable a model under Admin > Models (new models start disabled)."
	@echo "LLM responses are mocked. LM Studio on :1234 is picked up automatically if running."
	@echo ""
	@echo "Useful commands:"
	@echo "  make dev-status   Show pod status"
	@echo "  make dev-logs     Tail service logs"
	@echo "  make dev-test     Smoke-test the stack (DEV_EMAIL/DEV_PASSWORD for the full chat test)"

dev-down: ## Delete the local kind cluster
	@kind delete cluster --name $(DEV_CLUSTER) 2>/dev/null || true
	@echo "Local dev cluster deleted."

dev-build-gateway: ## Rebuild and redeploy the gateway service
	@docker build -t darkplum-gateway:latest services/gateway/
	@kind load docker-image --name $(DEV_CLUSTER) darkplum-gateway:latest
	@kubectl rollout restart deployment/darkplum-gateway -n darkplum-system
	@kubectl rollout status deployment/darkplum-gateway -n darkplum-system --timeout=60s

dev-build-api: ## Rebuild and redeploy the api service
	@docker build -t darkplum-api:latest services/api/
	@kind load docker-image --name $(DEV_CLUSTER) darkplum-api:latest
	@kubectl rollout restart deployment/darkplum-api -n darkplum-system
	@kubectl rollout status deployment/darkplum-api -n darkplum-system --timeout=60s

dev-build-web: ## Rebuild and redeploy the web frontend
	@docker build -t darkplum-web:latest services/web/
	@kind load docker-image --name $(DEV_CLUSTER) darkplum-web:latest
	@kubectl rollout restart deployment/darkplum-web -n darkplum-system
	@kubectl rollout status deployment/darkplum-web -n darkplum-system --timeout=60s

dev-status: ## Show local dev cluster status
	@echo "=== Pods ==="
	@kubectl get pods -n darkplum-system -o wide 2>/dev/null || echo "Cluster not running. Run: make dev-up"
	@echo ""
	@echo "=== Services ==="
	@kubectl get svc -n darkplum-system 2>/dev/null || true

dev-logs: ## Tail logs from all local dev services
	@echo "=== Gateway (last 20) ==="
	@kubectl logs -n darkplum-system deploy/darkplum-gateway --tail=20 2>/dev/null || true
	@echo ""
	@echo "=== API (last 20) ==="
	@kubectl logs -n darkplum-system deploy/darkplum-api --tail=20 2>/dev/null || true
	@echo ""
	@echo "=== Web (last 20) ==="
	@kubectl logs -n darkplum-system deploy/darkplum-web --tail=20 2>/dev/null || true

dev-test: ## Smoke-test the local stack [DEV_PORT=8080] [DEV_EMAIL=.. DEV_PASSWORD=.. for the authenticated chat test] [MODEL=..]
	@BASE=http://localhost:$(DEV_PORT); \
	H1='Content-Type: application/json'; H2='X-Requested-With: XMLHttpRequest'; \
	fail() { echo "  FAIL: $$1"; exit 1; }; \
	echo "Testing $$BASE ..."; \
	CODE=$$(curl -s -o /dev/null -w "%{http_code}" $$BASE/health); \
	[ "$$CODE" = "200" ] || fail "/health returned HTTP $$CODE (is the cluster running on DEV_PORT=$(DEV_PORT)?)"; \
	echo "  /health OK"; \
	CODE=$$(curl -s -o /dev/null -w "%{http_code}" $$BASE/v1/chat/completions -H "$$H1" -H "$$H2" \
		-d '{"model":"mock-model","messages":[{"role":"user","content":"Hello"}]}'); \
	[ "$$CODE" = "401" ] || fail "unauthenticated chat returned HTTP $$CODE, expected 401"; \
	echo "  unauthenticated chat is refused (401) OK"; \
	SETUP=$$(curl -s $$BASE/auth/config | jq -r '.setup_required'); \
	if [ "$$SETUP" = "true" ]; then \
		echo "  First-run setup not done yet: create your admin at $$BASE, then re-run with DEV_EMAIL/DEV_PASSWORD."; exit 0; \
	fi; \
	if [ -z "$$DEV_EMAIL" ] || [ -z "$$DEV_PASSWORD" ]; then \
		echo "  Skipping authenticated chat test. Run: DEV_EMAIL=you@example.com DEV_PASSWORD=... make dev-test"; exit 0; \
	fi; \
	JAR=$$(mktemp); trap 'rm -f $$JAR' EXIT; \
	CODE=$$(jq -n --arg e "$$DEV_EMAIL" --arg p "$$DEV_PASSWORD" '{email:$$e,password:$$p}' | \
		curl -s -o /dev/null -w "%{http_code}" -c $$JAR $$BASE/auth/login -H "$$H1" -H "$$H2" -d @-); \
	[ "$$CODE" = "200" ] || fail "login returned HTTP $$CODE"; \
	echo "  login OK"; \
	M="$(MODEL)"; \
	[ -n "$$M" ] || M=$$(curl -s -b $$JAR $$BASE/v1/models -H "$$H2" | jq -r '.data[0].id // empty'); \
	[ -n "$$M" ] || fail "no models are enabled. Enable one under Admin > Models."; \
	RESP=$$(jq -n --arg m "$$M" '{model:$$m,messages:[{role:"user",content:"Say hello in one sentence."}],max_tokens:50}' | \
		curl -s -w "\n%{http_code}" -b $$JAR $$BASE/v1/chat/completions -H "$$H1" -H "$$H2" -d @-); \
	CODE=$$(echo "$$RESP" | tail -1); BODY=$$(echo "$$RESP" | sed '$$d'); \
	[ "$$CODE" = "200" ] || fail "chat with '$$M' returned HTTP $$CODE: $$BODY"; \
	echo "  chat with '$$M' OK: $$(echo "$$BODY" | jq -r '.choices[0].message.content // empty' | head -c 120)"; \
	RESP=$$(curl -s -w "\n%{http_code}" -b $$JAR $$BASE/v1/chat/completions -H "$$H1" -H "$$H2" \
		-d '{"model":"darkplum-no-such-model","messages":[{"role":"user","content":"x"}]}'); \
	CODE=$$(echo "$$RESP" | tail -1); ERRCODE=$$(echo "$$RESP" | sed '$$d' | jq -r '.error.code // empty'); \
	[ "$$CODE" = "404" ] && [ "$$ERRCODE" = "model_not_found" ] || fail "unknown model returned HTTP $$CODE code='$$ERRCODE', expected 404 model_not_found"; \
	echo "  unknown model is refused (404 model_not_found) OK"; \
	echo "All checks passed."
