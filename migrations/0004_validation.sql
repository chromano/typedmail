-- Whether each extraction matched its inbox's full JSON Schema, and where it
-- didn't. Both are NULL for results stored before validation existed;
-- validation_errors is also NULL for a valid result.
ALTER TABLE extractions
    ADD COLUMN valid             boolean,
    ADD COLUMN validation_errors text[];
