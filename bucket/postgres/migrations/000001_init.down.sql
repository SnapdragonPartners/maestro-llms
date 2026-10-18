-- Reverts bucket schema v1: removes every bucket table and all limiter state.
DROP SCHEMA IF EXISTS bucket CASCADE;
