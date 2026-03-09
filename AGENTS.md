Skipping the obvious here, just check the code. it is a fairly standard Go project.

Important rules when updating the code:

- When -n/--dry-run is set, ensure that no state is modified. Caches and files can be read, but not written.
- Verify that you did not introduce any security issues or potential for unintended data loss with your changes.
- Ensure that README.md is still up to date after your changes.
- The help output must explain all constraints of what is and is not allowed to help AI agents use the tool. Examples are welcome.
- Whenever a behavior is unclear from protocol/docs, record it in `spec/open-questions.md` (create the file if it does not exist).
