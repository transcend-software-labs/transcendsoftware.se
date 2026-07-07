---
title: "Shipping AI-Generated Code with Confidence"
description: "The question with AI is no longer whether an agent can write the code. It's whether you can ship it. Here are the automated gates that let a team merge AI-written features without breaking production: OpenAPI breaking-change detection, SpotBugs, enforced coverage, and integration tests recorded from real third-party calls."
date: 2026-05-31
featured: true
tags: ["AI", "AI Agents", "MCP", "CI/CD", "GitHub Actions", "Testing", "Integration Tests", "OpenAPI", "SpotBugs", "Code Coverage", "Java"]
---

## The real bottleneck

The question with AI in engineering is no longer whether an agent can write the code. It can. The question is whether you can ship what it wrote without a senior engineer reading every line first.

If a human has to manually verify every diff, you have moved the bottleneck, not removed it. You have a faster typist and the same review queue.

The teams that actually move fast with AI are the ones that made correctness machine-verifiable. The agent writes the code; the pipeline decides whether it is allowed to merge. Confidence comes from the gates, not from trust. And the nice property of a gate is that it does not get tired, it does not skim, and it treats the agent's tenth pull request of the day exactly like the first.

Here is the setup I use to let a team -- including people who are not developers -- ship AI-written features into production without breaking it.

## Principle: every claim the agent makes must be checkable by a machine

An agent will tell you, confidently, that it did not change the API, that the new code is safe, and that it added tests. Sometimes that is true. The point is that you should never have to take its word for it. Each of those claims maps to a gate that runs in CI and fails the build when the claim is false.

Four gates carry most of the weight.

## Gate 1: breaking API changes, caught from the OpenAPI spec

Agents refactor freely. That is part of why they are useful, and part of why they will happily rename a field, change a status code, or tighten a type and not realize they just broke every consumer of your API.

You do not catch this by reading the diff. You catch it by generating the OpenAPI spec from the code on every pull request and diffing it against the spec on `main`. If the change is breaking, the build fails.

```yaml
- name: Check for breaking API changes
  uses: oasdiff/oasdiff-action/breaking@main
  with:
    base: main/openapi.yaml
    revision: build/openapi.yaml
    fail-on: ERR
```

The spec itself comes straight from the application -- for a Spring service, springdoc generates it at build time. The contract is no longer something a reviewer has to hold in their head. It is a file, and breaking it is a red build.

## Gate 2: static analysis with SpotBugs

LLMs reproduce the bug classes that are common in their training data: null dereferences, resource leaks, ignored return values, off-by-one boundary conditions. These are exactly the categories static analysis was built to find.

For Java, SpotBugs runs in the build and fails it on real findings:

```xml
<plugin>
    <groupId>com.github.spotbugs</groupId>
    <artifactId>spotbugs-maven-plugin</artifactId>
    <configuration>
        <effort>Max</effort>
        <threshold>Low</threshold>
        <failOnError>true</failOnError>
    </configuration>
    <executions>
        <execution>
            <goals>
                <goal>check</goal>
            </goals>
        </execution>
    </executions>
</plugin>
```

This is cheap to run and it catches the boring, mechanical mistakes before a human ever looks at the change. That is the whole idea: spend human attention on what is hard, let the machine reject what is mechanical.

## Gate 3: enforced test coverage

Agents are good at writing tests when you make them. Left alone, they will skip the test for the one branch that matters. So coverage is enforced, not requested. JaCoCo fails the build below 80% line coverage:

```xml
<plugin>
    <groupId>org.jacoco</groupId>
    <artifactId>jacoco-maven-plugin</artifactId>
    <executions>
        <execution>
            <id>check-coverage</id>
            <goals>
                <goal>check</goal>
            </goals>
            <configuration>
                <rules>
                    <rule>
                        <element>BUNDLE</element>
                        <limits>
                            <limit>
                                <counter>LINE</counter>
                                <value>COVEREDRATIO</value>
                                <minimum>0.80</minimum>
                            </limit>
                        </limits>
                    </rule>
                </rules>
            </configuration>
        </execution>
    </executions>
</plugin>
```

Coverage is a blunt instrument. It tells you that code was executed by a test, not that the test asserts anything meaningful. It is a floor, not a ceiling. Which brings us to the part that actually matters, and the part where AI struggles most.

## The hard part: integrating with a third-party system

Most of the features I build integrate with a large third-party system that speaks XML. This is where AI-generated code goes wrong, and where it goes wrong silently.

An agent does not know the real shape of a provider's responses. It guesses. The guess looks plausible, compiles, and passes the tests the agent wrote against its own guess. Everything is green and nothing works, because the test and the code agree on a contract that the provider never honored.

Worse, this breaks on iteration. You ship version one, it works by luck, and the next change quietly violates an assumption nobody wrote down.

The fix is a rule the agent must follow, with no exceptions:

> Never hand-write the expected XML. It must come from a real exchange.

The workflow looks like this:

1. Make one **real call** against the provider's test environment.
2. Record the exact request and response XML to a fixtures directory.
3. Build the integration test by replaying the recorded request and asserting against the recorded response.
4. From then on, the recorded exchange is the source of truth. The agent iterates against reality, not against its own assumptions.

```
src/test/resources/integration/
  create-order/
    request.xml      # exactly what we sent
    response.xml     # exactly what the provider returned
```

Now the integration test is grounded in a real interaction. When the agent refactors the integration three iterations later, the recorded exchange is a tripwire: if the new code stops producing the request the provider actually accepted, the test goes red. This single rule turned third-party integration from the least trustworthy part of the agent's output into one of the most trustworthy.

## Giving the agent the knowledge: a documentation MCP

The agent still needs to know how the provider's API works in the first place, and the provider's documentation is large, dense, and not something you want pasted into a prompt and half-remembered.

So I built an MCP server that exposes the provider's documentation as a tool the agent can query. Instead of guessing a message structure or a field name, the agent asks the MCP and gets the authoritative answer. Hallucinated field names mostly disappear, because the agent no longer has to invent what it can look up.

This is the difference between an agent that *sounds* like it knows the integration and one that actually does. The MCP is the knowledge; the recorded-exchange rule is the verification. You want both.

## Putting it together: the MCP in CI, and non-developers shipping features

The last step is where this stops being a personal productivity trick and becomes a team capability.

I integrated the documentation MCP into the GitHub-based agent workflow, so that agent sessions running there have access to it automatically. A developer does not have to set anything up locally. The knowledge of how to integrate with the provider is no longer in one engineer's head -- it is a tool that every agent session can reach.

The consequence is the part I did not fully expect. Once the agent has the provider's documentation on tap and a verification harness that forces real, recorded integration tests, the people *driving* the feature no longer have to be the people who memorized the provider's API. Someone who is not a developer can describe the feature they need, and the agent can implement an integration that is grounded in real calls and guarded by the same gates as everything else. The gates do not care who started the pull request. They only care whether it is correct.

## Why this is the whole point

None of these pieces is exotic. Spec diffing, static analysis, coverage thresholds, recorded fixtures, an MCP server. What makes them work together is the principle underneath: **shipping AI-written code is a question of "did it pass the gates," not "did a senior read every line."**

That is what lets a team move fast with AI without breaking production. Not faith in the model. Guardrails that make a mistake cheap to catch and impossible to merge.
