---
title: "Skepp — EU Infrastructure in One PR, and Why I Recommend It"
description: "I tested Skepp — a service that analyzes your repo, generates production Terraform and CI/CD, and opens a single PR to deploy your app to your own Scaleway account in the EU. Here's why I recommend it."
date: 2026-02-17
featured: true
tags: ["Skepp", "Terraform", "CI/CD", "Infrastructure as Code", "DevOps", "EU", "GDPR", "Scaleway", "GitHub", "Docker", "Deployment"]
---

## Introduction

If you've followed this blog, you know I care about owning my infrastructure. I've written about deploying full-stack apps with Terraform on DigitalOcean, and I've always preferred to keep everything as code, version-controlled, and reviewable. That's why when I came across Skepp, I was immediately interested -- and after testing it, I can genuinely recommend it.

Skepp is a service that installs as a GitHub App on your repository. It analyzes your codebase, detects your techstack and databases, and then opens a single pull request with production-ready Dockerfiles, GitHub Actions workflows, and Terraform configuration. Merge the PR, and your app deploys to your own Scaleway account in the EU.

## How it works

The workflow is refreshingly simple:

1. **Install** -- Add the Skepp GitHub App to your repository. One click, no config files.
2. **Review the PR** -- Skepp analyzes your code, detects your services and databases, and opens a PR with Dockerfiles, deploy workflows, and Terraform. Review it like any other PR.
3. **Merge & Go Live** -- Add your Scaleway API keys as repo secrets, merge the PR, and your app deploys automatically to the EU.

That's it. No YAML to write from scratch, no Terraform to learn, no Docker expertise required. Skepp handles the boilerplate and gives you a reviewable PR.

## Why I recommend it

### You own everything

This is the big one for me. I always prefer to own my infrastructure as code myself. The flexibility of being able to change stuff easily, and not being locked into a specific vendor, is something I value deeply.

With Skepp, everything it generates is standard Terraform, Docker, and GitHub Actions -- all committed to your repository. If you ever want to stop using Skepp, nothing breaks. Your CI/CD pipelines and Terraform config keep working independently. You can take your Dockerfile and deploy it anywhere. There's zero lock-in. This is fundamentally different from platforms like Vercel, Railway, or Render, where your app runs on their infrastructure and you're tied to their platform.

### EU-first and GDPR by default

All infrastructure deploys to Scaleway data centers in Paris or Amsterdam. Your data never leaves the European Union. For anyone building products that handle European user data, this is a huge win. Data sovereignty is built in from the start, not something you bolt on later.

### Deterministic, not AI-generated guesswork

Unlike AI code generators that produce Terraform that looks right but fails on `terraform apply`, Skepp uses deterministic heuristics. Your tech stack is identified from manifest files -- package.json, go.mod, pom.xml -- not guessed by a language model. The same repository produces the same infrastructure, every time. Every generated Terraform config passes `fmt`, `validate`, and `plan` against the real Scaleway API before it reaches your PR.

### Broad techstack support

Skepp auto-detects Spring Boot, Quarkus, Micronaut, Node.js, Go, and React/Next.js. It also detects database dependencies -- PostgreSQL, MySQL, Redis, and MongoDB -- from your manifests and adds managed database resources to your Terraform config automatically. Database users, passwords, and privileges are created and connection details injected as environment variables.

And if your stack isn't listed? Just add a Dockerfile. Skepp detects it, reads the EXPOSE port, and generates the deploy workflow and Terraform around it. Rust, Python, Elixir -- anything you can Dockerize, Skepp can deploy.

### Monorepo support

Single app or ten services in subdirectories -- Skepp discovers each one and generates per-service Dockerfiles and deploy workflows (triggered only on changes to that service's path), while Terraform is shared across all services.

### No middleman billing

You pay Scaleway directly for your cloud infrastructure -- no markup, no surprises. Skepp's own pricing is simple: a free tier for one repository, and a Pro plan at 29/month for unlimited repos, database infrastructure, re-analysis via @skepp mentions, and email support. There's a 14-day free trial on Pro with no credit card required.

### Git-native workflow

Everything lives in your repo. Infrastructure as code, reviewable in PRs. No dashboards, no clicking through UIs. This fits perfectly into the way I already work and the way I believe infrastructure should be managed.

## What the PR looks like

When Skepp opens the PR, it includes files like:

```
+ services/api/Dockerfile
+ services/frontend/Dockerfile
+ .github/workflows/deploy-api.yml
+ .github/workflows/deploy-frontend.yml
+ .github/workflows/terraform.yml
+ terraform/main.tf
+ terraform/variables.tf
+ terraform/terraform.tfvars
+ terraform/databases.tf
+ terraform/outputs.tf
```

The PR description includes a checklist of steps to complete before merging -- creating a Scaleway account, generating API keys, and adding secrets to your repository. It's well-structured and easy to follow.

## Conclusion

If you're a developer who wants to deploy to the EU, own your infrastructure as code, avoid vendor lock-in, and not spend days writing Terraform and CI/CD pipelines from scratch, I highly recommend giving Skepp a try. It solves a real problem -- bridging the gap between "I have code" and "I have production infrastructure" -- without taking ownership away from you. Everything is standard, reviewable, and ejectable. That's exactly how I want my infrastructure to work.
