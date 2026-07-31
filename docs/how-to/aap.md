# Set up AAP integration

This recipe walks through integrating DirQ with Ansible Automation Platform (AAP).

## Collection

```bash
cd collection/atgreen/dirq
ansible-galaxy collection build
ansible-galaxy collection install atgreen-dirq-1.0.0.tar.gz
```

Includes: `atgreen.dirq.dirq` inventory plugin + connection plugin.

## Execution Environment

```yaml
# execution-environment.yml
version: 3
dependencies:
  galaxy:
    collections:
      - name: atgreen.dirq
```

```bash
ansible-builder build -t dirq-ee:latest
```

## Credential Type

Import from `collection/atgreen/dirq/docs/aap-credential-type.yml` or create manually. Injects `DIRQ_SERVER_URL` and `DIRQ_TOKEN` as environment variables.

## Setup Checklist

1. Build and publish the `atgreen.dirq` collection
2. Build a custom EE and push to your registry
3. Import the DirQ credential type in AAP
4. Create DirQ credentials (one per DC if multi-DC)
5. Add inventory sources using `atgreen.dirq.dirq` plugin
6. Create job templates with `connection: atgreen.dirq.dirq`
7. Attach DirQ credentials to job templates
