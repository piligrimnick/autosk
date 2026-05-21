tasks:
- id
- author_id (fk:agents->id, nullable)
- title
- description
- workflow_id (fk:workflows->id, nullable)
- git_branch
- blocked_by
- status (enum: 'new', 'work', 'human', 'done', 'cancel')

agents:
- id
- name
- is_human (bool)

workflows:
- first_setp (fk:steps->id)
- description

steps:
- id
- agent_id (fk:agents->id)

steps_transitions:
- id
- step_id (fk:steps->id)
- next_step (fk:steps->id)
- prompt_rule

comments:
- id
- author (fk:agents->id)
- created_at
- task_id (fk:tasks->id)
- text
