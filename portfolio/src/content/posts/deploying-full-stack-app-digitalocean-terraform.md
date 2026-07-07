---
title: "Deploy a Full-Stack Application on DigitalOcean App Platform with Terraform"
description: "Learn how to easily deploy a scalable, cost-effective full-stack application on DigitalOcean App Platform using Terraform with full control using docker. Perfect for developers looking for easy and cheap cloud deployment."
date: 2024-06-02
tags: ["DigitalOcean", "Terraform", "Full-Stack", "Deployment", "Automation", "App Platform", "PostgreSQL", "Docker", "Cloud Hosting", "Database"]
---

## Introduction

Deploying a full-stack application (web, backend, and database) can be complex and time-consuming, especially with platforms like AWS or Google Cloud. These platforms often involve intricate setups, extensive configurations, and complex terminology that many developers find cumbersome. DigitalOcean's App Platform offers a simpler, more streamlined process. In this guide, we'll walk through deploying a scalable full-stack application using a Terraform script. This setup ensures ease of deployment and scalability, all while staying within a budget of $40/month, making it a cost-effective solution for production-ready deployments. Just FYI, I am not affiliated with DigitalOcean in any way.

## Prerequisites

Before we start, ensure you have the following:

- A DigitalOcean account
- Terraform installed on your machine
- Docker installed on your machine (needed for running commands to grant database permissions)
- A DigitalOcean API token to be used by terraform
- A DigitalOcean container registry
- Set up `DIGITALOCEAN_ACCESS_TOKEN` as a repository secret in GitHub for the GitHub Actions to work

Additionally, create a `terraform.tfvars.json` file with the following content:

```json
{
  "do_token": "your-do-token"
}
```

Replace `your-do-token` with your DigitalOcean API token.

## Setting up your terraform script

Here's the Terraform script we'll be using. This script automates the creation of a PostgreSQL database cluster, sets up a database user with permissions, and deploys a web and API service on DigitalOcean's App Platform.

```hcl
terraform {
  required_providers {
    digitalocean = {
      source  = "digitalocean/digitalocean"
      version = "~> 2.39.1"
    }
    null = {
      source  = "hashicorp/null"
      version = "~> 3.1.0"
    }
  }
}

provider "digitalocean" {
  token = var.do_token
}

variable "do_token" {}

variable "region" {
  description = "DO region"
  type        = string
  default     = "fra1"
}

variable "services_names" {
  description = "Name of the services you want to create"
  type        = object({
    web = string
    api = string
  })
  default = {
    "web" = "web"
    "api" = "api"
  }
}

variable "app_name" {
  description = "Name of the app"
  type        = string
  default     = "your-app-name"
}

variable "environments" {
  description = "Map of environment names and their attributes"
  type        = map(any)
  default = {
    "prod" = {
      "domain" : null,
      "db" : {
        "production" : true,
        "size" : "db-s-1vcpu-1gb",
        "node_count" : 1
      },
      "api" : {
        "instance_count" : 1,
        "size_slug" : "apps-s-1vcpu-0.5gb",
        "port" : 80
      },
      "web" : {
        "instance_count" : 1,
        "size_slug" : "basic-xxs",
        "port" : 80
      }
    }
  }
}

resource "digitalocean_database_cluster" "db-cluster" {
  for_each = var.environments
  name       = "${each.key}-cluster"
  engine     = "pg"
  version    = "16"
  size       = var.environments[each.key].db.size
  region     = var.region
  node_count = var.environments[each.key].db.node_count
}

resource "digitalocean_database_user" "api-user" {
  for_each   = var.environments
  cluster_id = digitalocean_database_cluster.db-cluster[each.key].id
  name       = "${var.services_names.api}-user"
}

resource "digitalocean_database_db" "api-db" {
  for_each   = var.environments
  cluster_id = digitalocean_database_cluster.db-cluster[each.key].id
  name       = "${var.services_names.api}-db"
}

resource "null_resource" "grant_permissions" {
  for_each = var.environments
  provisioner "local-exec" {
    command = <<EOT
      docker run --rm -e PGPASSWORD=${digitalocean_database_cluster.db-cluster[each.key].password} postgres:13 psql -h ${digitalocean_database_cluster.db-cluster[each.key].host} -U ${digitalocean_database_cluster.db-cluster[each.key].user} -p ${digitalocean_database_cluster.db-cluster[each.key].port} -d ${digitalocean_database_db.api-db[each.key].name} -c "GRANT ALL PRIVILEGES ON DATABASE \"${digitalocean_database_db.api-db[each.key].name}\" TO \"${digitalocean_database_user.api-user[each.key].name}\"; GRANT ALL PRIVILEGES ON SCHEMA public TO \"${digitalocean_database_user.api-user[each.key].name}\";"
    EOT
    environment = {
      PGPASSWORD = digitalocean_database_cluster.db-cluster[each.key].password
    }
  }
  depends_on = [
    digitalocean_database_cluster.db-cluster,
    digitalocean_database_user.api-user,
    digitalocean_database_db.api-db
  ]
}

resource "digitalocean_database_firewall" "db-cluster-fw" {
  for_each   = var.environments
  cluster_id = digitalocean_database_cluster.db-cluster[each.key].id
  rule {
    type  = "app"
    value = digitalocean_app.do-app[each.key].id
  }
  depends_on = [null_resource.grant_permissions]
}

resource "digitalocean_app" "do-app" {
  for_each = var.environments
  lifecycle {
    ignore_changes = [
      spec.0.features,
      spec.0.region,
      spec.0.service.0.image,
      spec.0.service.1.image
    ]
  }
  spec {
    name   = "${var.app_name}-${each.key}"
    region = var.region

    dynamic "domain" {
      for_each = var.environments[each.key].domain != null ? [1] : []
      content {
        name = var.environments[each.key].domain
      }
    }

    alert {
      rule = "DEPLOYMENT_FAILED"
    }

    service {
      name               = var.services_names.api
      instance_count     = var.environments[each.key].api.instance_count
      instance_size_slug = var.environments[each.key].api.size_slug

      image {
        registry_type = "DOCKER_HUB"
        repository    = "nginx"
        tag           = "latest"
      }

      http_port = var.environments[each.key].api.port

      env { key = "DB_PASSWORD"; value = digitalocean_database_user.api-user[each.key].password }
      env { key = "DB_HOST"; value = digitalocean_database_cluster.db-cluster[each.key].host }
      env { key = "DB_PORT"; value = digitalocean_database_cluster.db-cluster[each.key].port }
      env { key = "DB_NAME"; value = digitalocean_database_db.api-db[each.key].name }
      env { key = "DB_USER"; value = digitalocean_database_user.api-user[each.key].name }
    }

    service {
      name               = var.services_names.web
      instance_count     = var.environments[each.key].web.instance_count
      instance_size_slug = var.environments[each.key].web.size_slug

      image {
        registry_type = "DOCKER_HUB"
        repository    = "nginx"
        tag           = "latest"
      }

      http_port = var.environments[each.key].web.port
    }

    database {
      name         = digitalocean_database_db.api-db[each.key].name
      db_name      = digitalocean_database_db.api-db[each.key].name
      cluster_name = digitalocean_database_cluster.db-cluster[each.key].name
      production   = var.environments[each.key].db.production
    }

    ingress {
      rule {
        component { name = var.services_names.api }
        match { path { prefix = "/api" } }
      }
      rule {
        component { name = var.services_names.web }
        match { path { prefix = "/" } }
      }
    }
  }
}
```

### Explanation of the Terraform Script

**Variables:**
- `do_token`: Your DigitalOcean API token.
- `region`: The region where the resources will be created. Default is `fra1`.
- `services_names`: The names of the web and API services.
- `app_name`: The name of your application.
- `environment`: A map defining the different environments (e.g., prod) with their respective configurations.

**Resources:**
- **Database Cluster:** Creates a PostgreSQL database cluster with the specified configurations.
- **Database User:** Creates a database user for the API service.
- **Database:** Creates a database for the API service.
- **Grant Permissions:** Uses a Docker container to connect to the database and grant necessary permissions to the user.
- **Database Firewall:** Sets up a firewall to allow only the App Platform to access the database.
- **App:** Deploys the web and API services to the DigitalOcean App Platform with the specified configurations.

### Env variables

All required information for connecting to the created database will be set as environment variables in the api service automatically.

## Running the terraform script

1. **Initialize Terraform:** Run the following command to initialize your Terraform configuration.
   ```
   terraform init
   ```

2. **Apply the Configuration:** Apply the configuration to create your resources.
   ```
   terraform apply
   ```

3. **Review and Confirm:** Review the plan and confirm to proceed with the resource creation.

## GitHub actions for CI/CD

To automate the build and deployment process, you can use GitHub Actions. Below I will provide an example for a GitHub action to handle the web part of the code. Depending on what backend language you are using, you can set up something similar to handle the backend part as well. This GitHub Actions script is designed for a monorepo setup, expecting the frontend code to be in the `web` directory.

### GitHub Actions Workflow for Building and triggering deployment

This GitHub Actions workflow is designed to build and test your web service on every push to the main branch and on every pull request. For pull requests, it runs tests without deploying. On a push to the main branch, it also builds the Docker image, pushes it to the DigitalOcean Container Registry, and triggers a deployment.

Create a `.github/workflows/web.yml` file in your repository:

```yaml
name: web

on:
  push:
    branches:
      - main
    paths:
      - .github/workflows/web.yml
      - web/**
  pull_request:
    branches:
      - main
    paths:
      - .github/workflows/web.yml
      - web/**

concurrency:
  group: web-${{ github.event_name }}
  cancel-in-progress: false

jobs:
  build:
    runs-on: ubuntu-latest
    permissions: write-all
    env:
      SERVICE: web
      REGISTRY_NAME: your-registry-name
    outputs:
      version: ${{ steps.service_version.outputs.version }}
    steps:
      - name: Checkout sources
        uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - uses: actions/setup-node@v4
        with:
          node-version: 20
          cache: "yarn"
          cache-dependency-path: ${{ env.SERVICE }}/yarn.lock

      - name: Build ${{ env.SERVICE }}
        working-directory: ${{ env.SERVICE }}
        run: |
          yarn install
          yarn test
          yarn build

      - name: Install doctl
        if: github.event_name == 'push'
        uses: digitalocean/action-doctl@v2
        with:
          token: ${{ secrets.DIGITALOCEAN_ACCESS_TOKEN }}

      - name: Generate service version
        if: github.event_name == 'push'
        id: service_version
        uses: paulhatch/semantic-version@v5.4.0
        with:
          tag_prefix: ${{ env.SERVICE }}-v
          major_pattern: "(major-${{ env.SERVICE }}-bump)"
          minor_pattern: "(minor-${{ env.SERVICE }}-bump)"

      - name: Log in to DigitalOcean Container Registry
        if: github.event_name == 'push'
        run: doctl registry login --expiry-seconds 600

      - name: Build and push docker image
        if: github.event_name == 'push'
        working-directory: ${{ env.SERVICE }}
        run: |
          image_name=registry.digitalocean.com/${{ env.REGISTRY_NAME }}/${{ env.SERVICE }}:${{ steps.service_version.outputs.version }}
          docker build . -t $image_name
          docker push $image_name

      - name: Create tag
        if: github.event_name == 'push'
        uses: actions/github-script@v7
        with:
          script: |
            github.rest.git.createRef({
              owner: context.repo.owner,
              repo: context.repo.repo,
              ref: 'refs/tags/${{ env.SERVICE }}-v${{ steps.service_version.outputs.version }}',
              sha: context.sha
            })

  deploy:
    if: github.event_name == 'push'
    needs: build
    uses: ./.github/workflows/deploy-service.yml
    with:
      service: web
      version: ${{ needs.build.outputs.version }}
      env: prod
    secrets: inherit
```

### Deployment workflow

Create a `.github/workflows/deploy-service.yml` file in your repository:

```yaml
name: deploy-service
run-name: Deploy ${{ inputs.service }} ${{ inputs.version }} to ${{ inputs.env }}

on:
  workflow_call:
    inputs:
      service:
        required: true
        type: string
      version:
        required: true
        type: string
      env:
        required: true
        type: string
  workflow_dispatch:
    inputs:
      env:
        type: environment
        description: Environment to deploy to
      service:
        type: choice
        options:
          - api
          - web
        description: Service name
        required: true
      version:
        required: true
        description: Image version (e.g. 0.0.1)
        type: string

concurrency:
  group: deploy-${{ inputs.env }}
  cancel-in-progress: false

jobs:
  deploy-service:
    runs-on: ubuntu-latest
    environment: ${{ inputs.env }}
    steps:
      - name: Checkout sources
        uses: actions/checkout@v4

      - name: Deploy service
        uses: digitalocean/app_action@v1.1.6
        with:
          app_name: your-app-name-${{ inputs.env }}
          token: ${{ secrets.DIGITALOCEAN_ACCESS_TOKEN }}
          images: '[
            {
              "name": "${{ inputs.service }}",
              "image": {
                "registry_type": "DOCR",
                "repository": "${{ inputs.service }}",
                "tag": "${{ inputs.version }}"
              }
            }
          ]'
```

### Setting up secrets

Ensure you have set up a `DIGITALOCEAN_ACCESS_TOKEN` secret in your GitHub repository, available to the GitHub actions. This can be the same token as used above in Terraform, or a separate one if you would like. Best practice would be to separate them, as the GitHub action use-case will require less permissions to function.

## Additional steps

### Piping Logs to an External Service

To pipe logs to an external service like Logtail:

1. Create an account on Logtail and create a new source to get your source token.
2. Modify the Terraform Script to include Logtail configuration:

```hcl
variable "logtail_token_api" {}

resource "digitalocean_app" "do-app" {
  for_each = var.environments
  spec {
    service {
      name = var.services_names.api
      log_destination {
        name = "ApiLogs"
        logtail {
          token = var.logtail_token_api
        }
      }
    }
  }
}
```

3. Add `logtail_token_api` to your `terraform.tfvars.json`.

### Creating a Dev Environment

To create a dev environment, you can add a new entry to the `environments` variable:

```hcl
"dev" = {
  "domain" : null,
  "db" : {
    "production" : false,
    "size" : "db-s-1vcpu-1gb",
    "node_count" : 1,
  },
  "api" : {
    "instance_count" : 1,
    "size_slug" : "apps-s-1vcpu-0.5gb",
    "port" : 80
  },
  "web" : {
    "instance_count" : 1,
    "size_slug" : "basic-xxs",
    "port" : 80
  }
}
```

This will automatically create all resources in both test and production, with the given specifications.

### Adding a domain

Simply give the domain attributes a value and configure the rest through the DO portal. You might have to add `spec.0.domain` to the `ignore_changes` list after doing that.

### Scaling the API Vertically and Horizontally

**Vertical Scaling:** Modify the `size_slug` in your Terraform script:
```hcl
"size_slug" : "apps-s-1vcpu-1gb"
```

**Horizontal Scaling:** Modify the `instance_count` in your Terraform script:
```hcl
"instance_count": 2
```

## Conclusion

By following these steps, you can easily deploy and manage a full-stack application on DigitalOcean's App Platform, benefiting from the simplicity and cost-effectiveness of DigitalOcean compared to AWS or Google Cloud. This setup ensures that your application is production-ready and scalable while keeping costs under $40 per month.
