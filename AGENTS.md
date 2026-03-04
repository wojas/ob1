Skipping the obvious here, just check the code. it is a fairly standard Go project.

Important rules when updating the code:

- When -n/--dry-run is set, ensure that no state is modified. Caches and files can be read, but not written.
- Verify that you did not introduce any security issues or potential for unintended data loss with your changes.
- Ensure that README.md is still up to date after your changes.

