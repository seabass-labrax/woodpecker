<template>
  <Panel>
    <div class="flex flex-row border-b mb-4 pb-4 items-center dark:border-gray-600">
      <div class="ml-2">
        <h1 class="text-xl text-gray-500">Secrets</h1>
        <p class="text-sm text-gray-400 dark:text-gray-600">
          Secrets can be passed to individual pipeline steps at runtime as environmental variables.
          <DocsLink url="docs/usage/secrets" />
        </p>
      </div>
      <Button
        v-if="showAddSecret"
        class="ml-auto"
        text="Show secrets"
        start-icon="list"
        @click="showAddSecret = false"
      />
      <Button v-else class="ml-auto" text="Add secret" start-icon="plus" @click="showAddSecret = true" />
    </div>

    <div v-if="!showAddSecret" class="space-y-4 text-gray-500">
      <ListItem v-for="secret in secrets" :key="secret.id" class="items-center">
        <span>{{ secret.name }}</span>
        <div class="ml-auto">
          <span
            v-for="event in secret.event"
            :key="event"
            class="bg-gray-400 dark:bg-dark-200 dark:text-gray-500 text-white rounded-md mx-1 py-1 px-2 text-sm"
            >{{ event }}</span
          >
        </div>
        <IconButton
          icon="trash"
          class="ml-2 w-8 h-8 hover:text-red-400"
          :is-loading="isDeleting"
          @click="deleteSecret(secret)"
        />
      </ListItem>

      <div v-if="secrets?.length === 0" class="ml-2">There are no secrets yet.</div>
    </div>

    <div v-else class="space-y-4">
      <form @submit.prevent="createSecret">
        <InputField label="Name">
          <TextField v-model="selectedSecret.name" placeholder="Name" required />
        </InputField>

        <InputField label="Value">
          <TextField v-model="selectedSecret.value" placeholder="Value" required />
        </InputField>

        <InputField label="Available at following events">
          <CheckboxesField v-model="selectedSecret.event" :options="secretEventsOptions" />
        </InputField>

        <Button :is-loading="isSaving" type="submit" text="Add secret" />
      </form>
    </div>
  </Panel>
</template>

<script lang="ts">
import { defineComponent, inject, onMounted, Ref, ref } from 'vue';

import Button from '~/components/atomic/Button.vue';
import DocsLink from '~/components/atomic/DocsLink.vue';
import IconButton from '~/components/atomic/IconButton.vue';
import ListItem from '~/components/atomic/ListItem.vue';
import CheckboxesField from '~/components/form/CheckboxesField.vue';
import { CheckboxOption } from '~/components/form/form.types';
import InputField from '~/components/form/InputField.vue';
import TextField from '~/components/form/TextField.vue';
import Panel from '~/components/layout/Panel.vue';
import useApiClient from '~/compositions/useApiClient';
import { useAsyncAction } from '~/compositions/useAsyncAction';
import useNotifications from '~/compositions/useNotifications';
import { Repo, Secret, WebhookEvents } from '~/lib/api/types';

const emptySecret = {
  name: '',
  value: '',
  image: [],
  event: [WebhookEvents.Push],
};

const secretEventsOptions: CheckboxOption[] = [
  { value: WebhookEvents.Push, text: 'Push' },
  { value: WebhookEvents.Tag, text: 'Tag' },
  { value: WebhookEvents.PullRequest, text: 'Pull Request' },
  { value: WebhookEvents.Deploy, text: 'Deploy' },
];

export default defineComponent({
  name: 'SecretsTab',

  components: {
    Button,
    Panel,
    ListItem,
    IconButton,
    InputField,
    TextField,
    DocsLink,
    CheckboxesField,
  },

  setup() {
    const apiClient = useApiClient();
    const notifications = useNotifications();

    const repo = inject<Ref<Repo>>('repo');
    const secrets = ref<Secret[]>();
    const showAddSecret = ref(false);
    const selectedSecret = ref<Partial<Secret>>({ ...emptySecret });

    async function loadSecrets() {
      if (!repo?.value) {
        throw new Error("Unexpected: Can't load repo");
      }

      secrets.value = await apiClient.getSecretList(repo.value.owner, repo.value.name);
    }

    const { doSubmit: createSecret, isLoading: isSaving } = useAsyncAction(async () => {
      if (!repo?.value) {
        throw new Error("Unexpected: Can't load repo");
      }

      await apiClient.createSecret(repo.value.owner, repo.value.name, selectedSecret.value);
      notifications.notify({ title: 'Secret created', type: 'success' });
      showAddSecret.value = false;
      selectedSecret.value = { ...emptySecret };
      await loadSecrets();
    });

    const { doSubmit: deleteSecret, isLoading: isDeleting } = useAsyncAction(async (_secret: Secret) => {
      if (!repo?.value) {
        throw new Error("Unexpected: Can't load repo");
      }

      await apiClient.deleteSecret(repo.value.owner, repo.value.name, _secret.name);
      notifications.notify({ title: 'Secret deleted', type: 'success' });
      await loadSecrets();
    });

    onMounted(async () => {
      await loadSecrets();
    });

    return {
      secretEventsOptions,
      selectedSecret,
      secrets,
      showAddSecret,
      isSaving,
      isDeleting,
      createSecret,
      deleteSecret,
    };
  },
});
</script>
