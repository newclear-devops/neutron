-- Reference DDL for neutron's MySQL schema.
--
-- The authoritative schema is the GORM models in internal/repo.go: the API server
-- runs AutoMigrate on startup, which creates missing tables, columns and indexes.
-- This file mirrors what AutoMigrate emits for an empty database so a fresh
-- environment can also be provisioned by hand; keep it in sync when a model
-- changes. It is not a migration tool -- every statement is `if not exists`, so
-- running it against an existing database is a no-op.

create table if not exists neutron_project(
    id varchar(191) primary key,
    webhook_type longtext,
    repo_url longtext
);

create table if not exists neutron_job(
    id bigint primary key auto_increment,
    project_id varchar(191),
    name varchar(255),
    job_name varchar(255),
    status text,
    notify text,
    spec text,
    params text,
    completed boolean default false,
    completed_at datetime(3) null,
    unique index idx_neutron_job_name (name),
    index idx_job_project_name (project_id, job_name)
);

create table if not exists neutron_pod(
    id bigint primary key auto_increment,
    job_id bigint,
    pod_name varchar(255),
    pod_uid varchar(255),
    phase varchar(50),
    index idx_neutron_pod_job_id (job_id)
);

create table if not exists neutron_job_report(
    id bigint primary key auto_increment,
    job_name varchar(255),
    report_url varchar(2048),
    created_at datetime(3) null,
    unique index idx_neutron_job_report_job_name (job_name)
);

create table if not exists neutron_snippet(
    id bigint primary key auto_increment,
    name varchar(255),
    title varchar(255),
    content text,
    description text,
    params text,
    created_at datetime(3) null,
    updated_at datetime(3) null,
    unique index idx_neutron_snippet_name (name)
);

create table if not exists neutron_setting(
    `key` varchar(64) primary key,
    `value` longtext,
    updated_at datetime(3) null
);
