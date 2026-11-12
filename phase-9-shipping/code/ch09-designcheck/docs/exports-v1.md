# ExportHub: next-generation export platform

*An invented document, written badly on purpose for Chapter 9.9. It is an extract: the original in the chapter's story ran to 28 pages.*

## Overview

ExportHub is a scalable, flexible and robust microservice platform that will power all export use cases going forward. It leverages best-in-class technologies to deliver a seamless, enterprise-grade experience, and it provides a future-proof foundation for advanced reporting features as needed.

## Architecture

ExportHub consists of ten microservices: Export Gateway, Export Orchestrator, Export Scheduler, CSV Format Service, PDF Format Service, XLSX Format Service, Template Service, Delivery Service, Notification Bridge and Export Metadata Service.

Each service owns its own database for maximum decoupling. Services communicate through Kafka topics, which ensures high throughput and fault tolerance. The Orchestrator implements a saga across the format services, and the Scheduler supports cron expressions for recurring exports.

All services are stateless and highly available, and scale horizontally as needed. A service mesh provides secure and performant communication between them.

## Technology choices

- Kafka, because it is highly scalable and is the industry standard for event streaming.
- MongoDB for export metadata, because of its flexible schema.
- Redis for caching templates, because it is fast.
- Kubernetes operators for each data store, following best practice.

## Data model

An `ExportJob` has an id, a tenant, a type, a format, a template id, a schedule, a status, a set of filters, a delivery method, a notification preference, a priority, a retry policy, a compression setting, a locale, and a list of output artefacts. Each artefact has an id, a location, a checksum, a size, a content type and an expiry policy. (Fourteen further tables follow in the full document.)

## API

`POST /v1/exporthub/jobs`, `GET /v1/exporthub/jobs/{id}`, `GET /v1/exporthub/jobs/{id}/artefacts`, `POST /v1/exporthub/schedules`, `GET /v1/exporthub/templates`, and so on. The API is simple and intuitive.

## Timeline

- Phase 1, third quarter: the core platform.
- Phase 2, fourth quarter: migrate existing exports.
- Phase 3: advanced features, as needed.

## Conclusion

ExportHub will be a robust and scalable foundation for the future of exports.
