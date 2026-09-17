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

PROD_TARGETS := deploy deploy-infra deploy-k8s deploy-platform build-images push-images configure-sso test status destroy
ifneq ($(filter $(PROD_TARGETS),$(MAKECMDGOALS)),)
# Read from the normalized output contract — never reconstruct cloud-specific
# values (e.g. the ECR host from `aws sts get-caller-identity`) in the Makefile.
# Use -json|jq, not -raw: `terraform output -raw` prints a multi-line "No outputs
# found" warning to STDOUT (exit 0) before apply, which -raw would capture as the
# value. -json yields "{}" so these stay empty and the _require-outputs guard fires.
PROVIDER_REGION := $(shell cd $(TF_DIR) && terraform output -json 2>/dev/null | jq -r '.provider_region.value // empty')
CONTAINER_REGISTRY := $(shell cd $(TF_DIR) && terraform output -json 2>/dev/null | jq -r '.container_registry.value // empty')
endif

export DOCKER_CLI_HINTS = false
export DOCKER_CLI_HOOKS = 0

.PHONY: help check-prereqs _require-outputs deploy deploy-infra deploy-k8s deploy-platform build-images push-images configure-sso test status destroy \
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

deploy-k8s: ## Deploy Kubernetes workloads (vLLM inference)
	@echo "Applying namespaces..."
	@kubectl apply -f $(K8S_DIR)/base/namespaces/darkplum-inference.yaml
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
		sed -e "s|PLACEHOLDER_MODEL_NAME|$$MODEL_NAME|g" \
			-e "s|PLACEHOLDER_SERVED_MODEL_NAME|$$SERVED_NAME|g" \
			$(K8S_DIR)/overlays/prod/resources/vllm-deployment.yaml | kubectl apply -f -
	@kubectl apply -f $(K8S_DIR)/overlays/prod/resources/vllm-service.yaml
	@kubectl apply -f $(K8S_DIR)/base/network-policies/vllm-ingress.yaml
	@echo "vLLM deployment created. Model download + loading takes 15-25 minutes."
	@echo "Monitor progress: kubectl logs -f -n $(NAMESPACE) deploy/vllm-inference -c model-download"

deploy: deploy-infra deploy-k8s deploy-platform ## Full deploy: infrastructure + workloads + platform
	@echo ""
	@echo "========================================="
	@echo "  Darkplum deployment complete."
	@echo "========================================="
	@echo ""
	@echo "Wait for vLLM to finish loading, then run:"
	@echo "  make test"
	@echo ""
	@echo "Monitor model loading:"
	@echo "  kubectl logs -f -n $(NAMESPACE) deploy/vllm-inference -c model-download"
	@echo "  kubectl logs -f -n $(NAMESPACE) deploy/vllm-inference -c vllm"
	@echo ""
	@echo "Check readiness:"
	@echo "  make status"

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
			--dry-run=client -o yaml | kubectl apply -f -
	@echo "Deploying gateway..."
	@sed "s|darkplum-gateway:latest|$(CONTAINER_REGISTRY)/darkplum-gateway:latest|" \
		$(K8S_DIR)/base/gateway/deployment.yaml | kubectl apply -f -
	@kubectl apply -f $(K8S_DIR)/base/gateway/service.yaml
	@echo "Deploying api..."
	@sed "s|darkplum-api:latest|$(CONTAINER_REGISTRY)/darkplum-api:latest|" \
		$(K8S_DIR)/base/api/deployment.yaml | kubectl apply -f -
	@kubectl apply -f $(K8S_DIR)/base/api/service.yaml
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
		DEFAULT_REDIRECT="http://$$WEB_URL/auth/callback"; \
		read -p "OIDC Redirect URL [$$DEFAULT_REDIRECT]: " OIDC_REDIRECT_URL; \
		OIDC_REDIRECT_URL=$${OIDC_REDIRECT_URL:-$$DEFAULT_REDIRECT}; \
	fi; \
	EXISTING_SESSION_SIGNING_KEY=$$(kubectl get secret darkplum-auth -n darkplum-system \
		-o jsonpath='{.data.SESSION_SIGNING_KEY}' 2>/dev/null | base64 -d 2>/dev/null); \
	SESSION_SIGNING_KEY=$${EXISTING_SESSION_SIGNING_KEY:-$$(openssl rand -base64 48)}; \
	echo ""; \
	echo "Updating darkplum-auth secret..."; \
	kubectl create secret generic darkplum-auth \
		--namespace=darkplum-system \
		--from-literal=OIDC_ISSUER="$$OIDC_ISSUER" \
		--from-literal=OIDC_CLIENT_ID="$$OIDC_CLIENT_ID" \
		--from-literal=OIDC_CLIENT_SECRET="$$OIDC_CLIENT_SECRET" \
		--from-literal=OIDC_REDIRECT_URL="$$OIDC_REDIRECT_URL" \
		--from-literal=SESSION_SIGNING_KEY="$$SESSION_SIGNING_KEY" \
		--dry-run=client -o yaml | kubectl apply -f -; \
	echo "Restarting darkplum-api to pick up new config..."; \
	kubectl rollout restart deployment/darkplum-api -n darkplum-system; \
	kubectl rollout status deployment/darkplum-api -n darkplum-system --timeout=120s; \
	echo ""; \
	echo "SSO configured. Users will now see 'Sign in with SSO' at $$OIDC_REDIRECT_URL"; \
	echo ""; \
	echo "IMPORTANT: Add this redirect URL to your identity provider:"; \
	echo "  $$OIDC_REDIRECT_URL"

test: ## Send a test chat completion request to vLLM
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
	@kubectl cluster-info 2>/dev/null || echo "kubeconfig not set. Run: make deploy-infra"
	@echo ""
	@echo "=== Nodes ==="
	@kubectl get nodes -L darkplum.ai/node-type 2>/dev/null || true
	@echo ""
	@echo "=== GPU Allocatable ==="
	@kubectl get nodes -l darkplum.ai/node-type=gpu-inference \
		-o custom-columns="NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu,STATUS:.status.conditions[-1].type" \
		2>/dev/null || echo "No GPU nodes found."
	@echo ""
	@echo "=== Pods ==="
	@kubectl get pods -n $(NAMESPACE) -o wide 2>/dev/null || true
	@echo ""
	@echo "=== vLLM Logs (last 5 lines) ==="
	@kubectl logs -n $(NAMESPACE) deploy/vllm-inference -c vllm --tail=5 2>/dev/null || \
		kubectl logs -n $(NAMESPACE) deploy/vllm-inference -c model-download --tail=5 2>/dev/null || \
		echo "No vLLM pods running."

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

# =============================================================================
# Local Development (kind)
# =============================================================================

dev-up: ## Create local kind cluster and deploy all services
	@command -v kind >/dev/null 2>&1 || { echo "ERROR: kind not found. Install: https://kind.sigs.k8s.io/docs/user/quick-start/#installation"; exit 1; }
	@command -v kubectl >/dev/null 2>&1 || { echo "ERROR: kubectl not found."; exit 1; }
	@command -v docker >/dev/null 2>&1 || { echo "ERROR: docker not found."; exit 1; }
	@if kind get clusters 2>/dev/null | grep -q "^$(DEV_CLUSTER)$$"; then \
		echo "Cluster $(DEV_CLUSTER) already exists. Run 'make dev-down' first to recreate."; \
		exit 1; \
	fi
	@echo "Creating kind cluster..."
	@TMPCONFIG=$$(mktemp) && \
		sed "s|\$${HOME}|$$HOME|" $(K8S_DIR)/overlays/dev/kind-config.yaml > $$TMPCONFIG && \
		kind create cluster --name $(DEV_CLUSTER) --config $$TMPCONFIG; \
		rm -f $$TMPCONFIG
	@echo "Building gateway image..."
	@docker build -t darkplum-gateway:latest services/gateway/
	@echo "Building api image..."
	@docker build -t darkplum-api:latest services/api/
	@echo "Building web image..."
	@docker build -t darkplum-web:latest services/web/
	@echo "Loading images into kind..."
	@kind load docker-image --name $(DEV_CLUSTER) darkplum-gateway:latest darkplum-api:latest darkplum-web:latest
	@echo "Deploying all services..."
	@kubectl apply -k $(K8S_DIR)/overlays/dev
	@echo "Waiting for services to be ready..."
	@sleep 5
	@kubectl wait --namespace=darkplum-system --for=condition=ready pod -l app.kubernetes.io/name=postgres --timeout=60s
	@kubectl wait --namespace=darkplum-system --for=condition=ready pod -l app.kubernetes.io/name=darkplum-gateway --timeout=90s
	@kubectl wait --namespace=darkplum-system --for=condition=ready pod -l app.kubernetes.io/name=darkplum-api --timeout=90s
	@kubectl wait --namespace=darkplum-system --for=condition=ready pod -l app.kubernetes.io/name=darkplum-web --timeout=60s
	@echo ""
	@echo "========================================="
	@echo "  Local dev cluster ready."
	@echo "  http://localhost:8080"
	@echo "========================================="
	@echo ""
	@echo "First run: create an admin account at http://localhost:8080"
	@echo "LLM responses are mocked."
	@echo ""
	@echo "Useful commands:"
	@echo "  make dev-status   Show pod status"
	@echo "  make dev-logs     Tail service logs"
	@echo "  make dev-test     Send a test chat request"

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

dev-test: ## Send a test chat completion through the full local stack
	@echo "Testing health endpoint..."
	@HTTP_CODE=$$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/health); \
	if [ "$$HTTP_CODE" != "200" ]; then \
		echo "FAIL: /health returned HTTP $$HTTP_CODE (is the cluster running?)"; \
		exit 1; \
	fi; \
	echo "  /health OK"
	@echo "Testing chat completions (end-to-end)..."
	@RESPONSE=$$(curl -s -w "\n%{http_code}" http://localhost:8080/v1/chat/completions \
		-H "Content-Type: application/json" \
		-H "X-Requested-With: XMLHttpRequest" \
		-d '{"model": "mock-model", "messages": [{"role": "user", "content": "Hello"}]}'); \
	HTTP_CODE=$$(echo "$$RESPONSE" | tail -1); \
	BODY=$$(echo "$$RESPONSE" | sed '$$d'); \
	if [ "$$HTTP_CODE" = "200" ]; then \
		CONTENT=$$(echo "$$BODY" | jq -r '.choices[0].message.content // empty'); \
		echo "  SUCCESS: HTTP $$HTTP_CODE"; \
		echo "  Response: $$CONTENT"; \
	else \
		echo "  FAIL: HTTP $$HTTP_CODE"; \
		echo "  Body: $$BODY"; \
		exit 1; \
	fi
