-- Records the redirect URI an instance's OAuth client was registered with.
--
-- The client_id an instance issues is bound to the redirect URI given at
-- registration. Without this column the app is cached by host alone, so
-- changing FR_BASE_URL — a new domain, a different port — leaves every login
-- redirecting to an instance that rejects the request, with nothing in this
-- service's logs to say why. Existing rows get an empty value, which reads as
-- "unknown" and forces one re-registration.
ALTER TABLE instances ADD COLUMN redirect_uri TEXT NOT NULL DEFAULT '';
