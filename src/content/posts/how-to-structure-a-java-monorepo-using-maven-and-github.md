---
title: "How to Structure a Java Monorepo using Maven and Github"
description: "A practical guide to structuring a Java monorepo using Maven and GitHub Actions. Covers project layout, shared libraries, reusable CI/CD workflows, and how to build only what changed — without a remote artifactory."
date: 2023-01-01
tags: ["Java", "Maven", "Github", "Github actions", "Monorepo", "CI", "CD"]
featured: true
ogImage: "/maven_github_monorepo.png"
---

## Why Monorepo?

While Microservice architectures solves a lot of issues, it also brings challenges. One of them being scattered code, in different repositories. Monorepos can solve that, by bringing the code of different projects together in one repository.

Imagine that you want to create a library which different services can reuse. Instead of creating a separate repository for that, you create a module in the Monorepo. Then you inject the library in the service `pom.xml` file:

```xml
<dependency>
    <groupId>org.example</groupId>
    <artifactId>library</artifactId>
    <version>${project.version}</version>
</dependency>
```

You are now referring to the library in the local repository. I see the following advantages with this:

- You don't need a remote artifactory for your library
- When doing a change to a library, you can validate that all services is compatible with the change before letting that change into main
- You can build a new version of every dependent service when committing a change to a library

Imagine merging a code change of the library to main, having to wait for the new version of it being published to your artifactory, go into all dependent services own repositories and bump to the new version and commit all of them separately only to discover that the 4th service is not compatible.

Instead, you only do one change to the monorepo and get a new version for each service from the same commit.

To read more about Monorepos and the benefits and challenges of it (because it's a tradeoff as everything else) I'd suggest reading [this](https://semaphoreci.com/blog/what-is-monorepo).

Now let's get to it.

## File structure

```
📦 project
 ┣ 📂 .github
 ┃ ┗ 📂 workflows
 ┃   ┗ 📜 service1.yml
 ┃   ┗ 📜 service2.yml
 ┃   ┗ 📜 service-workflow.yml
 ┣ 📂 libs
 ┃ ┗ 📂 lib1
 ┃   ┗ 📂 src
 ┃   ┗ 📜 pom.xml
 ┃ ┗ 📂 lib2
 ┃   ┗ 📂 src
 ┃   ┗ 📜 pom.xml
 ┣ 📂 services
 ┃ ┗ 📂 service1
 ┃   ┗ 📂 src
 ┃   ┗ 📜 pom.xml
 ┃ ┗ 📂 service2
 ┃   ┗ 📂 src
 ┃   ┗ 📜 pom.xml
 ┣ 📜 pom.xml
 ┗ 📜 Dockerfile
```

The illustration above is a good structure for your Monorepo.

## Services & libs

You have two separate modules, services and libs. The services module will contain all your services, while your libs module will contain all your libraries. The services module `pom.xml` file can contain common logic that should apply to all services, while the libs `pom.xml` file can contain common logic that should apply to all libs.

## Dockerfile

A dockerfile that can be reused by all services. It can look something like this:

```dockerfile
FROM eclipse-temurin:17-jdk-alpine
ARG JAR_FILE
COPY ${JAR_FILE} app.jar
ENTRYPOINT java -jar app.jar
```

It accepts the path of the JAR file as an argument. This will be useful in our github workflow.

## Github workflows

Contains all workflows for your services. `service-workflow.yml` is supposed to be reusable for all services. It should look something like this:

```yaml
on:
  workflow_call:
    inputs:
      service:
        required: true
        type: string

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout sources
        uses: actions/checkout@v3

      - name: Set up JDK 17
        uses: actions/setup-java@v3
        with:
          java-version: "17"
          distribution: "adopt"

      - name: Build application
        run: mvn clean package --projects :${{ inputs.service }} --also-make --batch-mode

      - name: Build docker image
        run: docker build . --build-arg JAR_FILE=services/${{ inputs.service }}/target/app.jar -t ${{ github.sha }}
```

The above workflow accepts the service name as input, then proceeds to:

1. Check out the repository
2. Set up JDK 17 and builds the service in question

`mvn clean package --projects :${{ inputs.service }} --also-make --batch-mode` builds the service along with all of its dependencies. This is important, as it mitigates the need for uploading libraries to an artifactory and injecting them from there. This is one of the big upsides with using a monorepo.

3. Build a docker image

Notice that we are expecting the jar file of all services to be named `app.jar`. For this to work, some config in the `pom.xml` file of the services module is needed:

```xml
<build>
    <finalName>app</finalName>
</build>
```

The above configuration will make sure that the name will be `app.jar` of all services.

You probably want to push this docker image to an artifactory as well. For simplicity I left that part out, as it depends on which provider you are using.

You can then reuse the `service-workflow.yml` file from all individual services workflows, like so:

```yaml
name: service1

on:
  push:
    branches: [main]
    paths:
      - services/service1/**
      - .github/workflows/service1.yml

jobs:
  run:
    uses: ./.github/workflows/service-workflow.yml
    with:
      service: service1
```

The above workflow will trigger on every push that changes a file underneath `services/service1/**` or the workflow itself placed at `.github/workflows/service1.yml` and will invoke `service-workflow.yml` with `service1` as input for the service name.

If service1 would be dependent on let's say `lib1`, you would want to add the following path to `paths`:

```yaml
- libs/lib1/**
```

The workflow for service1 would then trigger on any change to lib1 as well.

## Complete example

A complete example can be found [here](https://github.com/razum90/maven-monorepo-example).
