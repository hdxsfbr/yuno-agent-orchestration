-- Delivery process wave 4 review (L2): a hash of the title and text a ticket's issue was last
-- written from, so that a revision can tell a ticket whose text changed from one that did not
-- without reading the tracker. Null for tickets published before this column; those are
-- compared by title only.
ALTER TABLE plan_tickets ADD COLUMN content_hash TEXT;
