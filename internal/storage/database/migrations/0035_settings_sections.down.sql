-- Reverses v35. The release before reads only the blob tables, which this release kept writing, so nothing it
-- reads is lost; section versions and the fields only the section rows kept are.
DROP TABLE IF EXISTS settings_sections;
