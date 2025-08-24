package web

const dashboardTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Refity Docker Registry Dashboard</title>
    <script src="https://cdn.tailwindcss.com"></script>
    <script src="https://unpkg.com/alpinejs@3.x.x/dist/cdn.min.js" defer></script>
    <link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/font-awesome/6.0.0/css/all.min.css">
</head>
<body class="bg-gray-50">
    <div x-data="dashboard()" class="min-h-screen">
        <!-- Header -->
        <header class="bg-white shadow-sm border-b">
            <div class="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
                <div class="flex justify-between items-center py-6">
                    <div class="flex items-center">
                        <i class="fas fa-docker text-blue-600 text-2xl mr-3"></i>
                        <h1 class="text-2xl font-bold text-gray-900">Refity Registry Dashboard</h1>
                    </div>
                    <div class="flex items-center space-x-4">
                        <span class="text-sm text-gray-500">Total Images: {{.TotalImages}}</span>
                        <button @click="refreshData()" class="bg-blue-600 hover:bg-blue-700 text-white px-4 py-2 rounded-md text-sm font-medium">
                            <i class="fas fa-sync-alt mr-2"></i>Refresh
                        </button>
                    </div>
                </div>
            </div>
        </header>

        <!-- Main Content -->
        <main class="max-w-7xl mx-auto py-6 sm:px-6 lg:px-8">
            <!-- Stats Cards -->
            <div class="grid grid-cols-1 md:grid-cols-3 gap-6 mb-8">
                <div class="bg-white overflow-hidden shadow rounded-lg">
                    <div class="p-5">
                        <div class="flex items-center">
                            <div class="flex-shrink-0">
                                <i class="fas fa-layer-group text-blue-600 text-2xl"></i>
                            </div>
                            <div class="ml-5 w-0 flex-1">
                                <dl>
                                    <dt class="text-sm font-medium text-gray-500 truncate">Total Repositories</dt>
                                    <dd class="text-lg font-medium text-gray-900">{{len .Repositories}}</dd>
                                </dl>
                            </div>
                        </div>
                    </div>
                </div>

                <div class="bg-white overflow-hidden shadow rounded-lg">
                    <div class="p-5">
                        <div class="flex items-center">
                            <div class="flex-shrink-0">
                                <i class="fas fa-tags text-green-600 text-2xl"></i>
                            </div>
                            <div class="ml-5 w-0 flex-1">
                                <dl>
                                    <dt class="text-sm font-medium text-gray-500 truncate">Total Images</dt>
                                    <dd class="text-lg font-medium text-gray-900">{{.TotalImages}}</dd>
                                </dl>
                            </div>
                        </div>
                    </div>
                </div>

                <div class="bg-white overflow-hidden shadow rounded-lg">
                    <div class="p-5">
                        <div class="flex items-center">
                            <div class="flex-shrink-0">
                                <i class="fas fa-hdd text-purple-600 text-2xl"></i>
                            </div>
                                                            <div class="ml-5 w-0 flex-1">
                                    <dl>
                                        <dt class="text-sm font-medium text-gray-500 truncate">Total Size</dt>
                                        <dd class="text-lg font-medium text-gray-900">{{formatBytes .TotalSize}}</dd>
                                    </dl>
                                </div>
                        </div>
                    </div>
                </div>
            </div>

            <!-- Repositories List -->
            <div class="bg-white shadow overflow-hidden sm:rounded-md">
                <div class="px-4 py-5 sm:px-6 border-b border-gray-200">
                    <h3 class="text-lg leading-6 font-medium text-gray-900">Docker Repositories</h3>
                    <p class="mt-1 max-w-2xl text-sm text-gray-500">Manage your Docker images and tags</p>
                </div>
                
                <div class="divide-y divide-gray-200">
                    {{range .Repositories}}
                    <div class="px-4 py-4 sm:px-6 hover:bg-gray-50">
                        <div class="flex items-center justify-between">
                            <div class="flex items-center">
                                <i class="fas fa-box text-blue-500 mr-3"></i>
                                <div>
                                    <h4 class="text-lg font-medium text-gray-900">{{.Name}}</h4>
                                    <div class="mt-1 flex flex-wrap gap-2">
                                        {{range .Tags}}
                                        <span class="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-blue-100 text-blue-800">
                                            {{.Name}} ({{formatBytes .Size}})
                                            <button @click="deleteTag('{{$.Name}}', '{{.Name}}')" class="ml-1 text-blue-600 hover:text-blue-800">
                                                <i class="fas fa-times"></i>
                                            </button>
                                        </span>
                                        {{end}}
                                    </div>
                                </div>
                            </div>
                            <div class="flex items-center space-x-2">
                                <span class="text-sm text-gray-500">{{len .Tags}} tags</span>
                                <button @click="deleteRepository('{{.Name}}')" class="text-red-600 hover:text-red-800 text-sm font-medium">
                                    <i class="fas fa-trash mr-1"></i>Delete
                                </button>
                            </div>
                        </div>
                    </div>
                    {{end}}
                </div>

                {{if eq (len .Repositories) 0}}
                <div class="text-center py-12">
                    <i class="fas fa-box-open text-gray-400 text-4xl mb-4"></i>
                    <h3 class="text-lg font-medium text-gray-900 mb-2">No repositories found</h3>
                    <p class="text-gray-500">Start by pushing some Docker images to your registry</p>
                </div>
                {{end}}
            </div>
        </main>

        <!-- Notification Toast -->
        <div x-show="notification.show" x-transition class="fixed top-4 right-4 z-50">
            <div :class="notification.type === 'success' ? 'bg-green-500' : 'bg-red-500'" class="text-white px-6 py-4 rounded-lg shadow-lg">
                <div class="flex items-center">
                    <i :class="notification.type === 'success' ? 'fas fa-check-circle' : 'fas fa-exclamation-circle'" class="mr-2"></i>
                    <span x-text="notification.message"></span>
                </div>
            </div>
        </div>

        <!-- Confirmation Modal -->
        <div x-show="modal.show" x-transition class="fixed inset-0 z-50 overflow-y-auto" style="display: none;">
            <div class="flex items-end justify-center min-h-screen pt-4 px-4 pb-20 text-center sm:block sm:p-0">
                <div class="fixed inset-0 bg-gray-500 bg-opacity-75 transition-opacity"></div>
                <div class="inline-block align-bottom bg-white rounded-lg text-left overflow-hidden shadow-xl transform transition-all sm:my-8 sm:align-middle sm:max-w-lg sm:w-full">
                    <div class="bg-white px-4 pt-5 pb-4 sm:p-6 sm:pb-4">
                        <div class="sm:flex sm:items-start">
                            <div class="mx-auto flex-shrink-0 flex items-center justify-center h-12 w-12 rounded-full bg-red-100 sm:mx-0 sm:h-10 sm:w-10">
                                <i class="fas fa-exclamation-triangle text-red-600"></i>
                            </div>
                            <div class="mt-3 text-center sm:mt-0 sm:ml-4 sm:text-left">
                                <h3 class="text-lg leading-6 font-medium text-gray-900" x-text="modal.title"></h3>
                                <div class="mt-2">
                                    <p class="text-sm text-gray-500" x-text="modal.message"></p>
                                </div>
                            </div>
                        </div>
                    </div>
                    <div class="bg-gray-50 px-4 py-3 sm:px-6 sm:flex sm:flex-row-reverse">
                        <button @click="confirmAction()" type="button" class="w-full inline-flex justify-center rounded-md border border-transparent shadow-sm px-4 py-2 bg-red-600 text-base font-medium text-white hover:bg-red-700 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-red-500 sm:ml-3 sm:w-auto sm:text-sm">
                            Confirm
                        </button>
                        <button @click="closeModal()" type="button" class="mt-3 w-full inline-flex justify-center rounded-md border border-gray-300 shadow-sm px-4 py-2 bg-white text-base font-medium text-gray-700 hover:bg-gray-50 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-indigo-500 sm:mt-0 sm:ml-3 sm:w-auto sm:text-sm">
                            Cancel
                        </button>
                    </div>
                </div>
            </div>
        </div>
    </div>

    <script>
        // Format bytes to human readable format
        function formatBytes(bytes) {
            if (bytes === 0) return '0 Bytes';
            const k = 1024;
            const sizes = ['Bytes', 'KB', 'MB', 'GB', 'TB'];
            const i = Math.floor(Math.log(bytes) / Math.log(k));
            return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
        }

        function dashboard() {
            return {
                notification: {
                    show: false,
                    message: '',
                    type: 'success'
                },
                modal: {
                    show: false,
                    title: '',
                    message: '',
                    action: null
                },

                showNotification(message, type = 'success') {
                    this.notification.message = message;
                    this.notification.type = type;
                    this.notification.show = true;
                    setTimeout(() => {
                        this.notification.show = false;
                    }, 3000);
                },

                showModal(title, message, action) {
                    this.modal.title = title;
                    this.modal.message = message;
                    this.modal.action = action;
                    this.modal.show = true;
                },

                closeModal() {
                    this.modal.show = false;
                    this.modal.action = null;
                },

                confirmAction() {
                    if (this.modal.action) {
                        this.modal.action();
                    }
                    this.closeModal();
                },

                async refreshData() {
                    try {
                        const response = await fetch('/api/repositories');
                        if (response.ok) {
                            location.reload();
                        } else {
                            this.showNotification('Failed to refresh data', 'error');
                        }
                    } catch (error) {
                        this.showNotification('Failed to refresh data', 'error');
                    }
                },

                deleteTag(repo, tag) {
                    this.showModal(
                        'Delete Tag',
                        'Are you sure you want to delete tag "' + tag + '" from repository "' + repo + '"?',
                        () => this.performDeleteTag(repo, tag)
                    );
                },

                async performDeleteTag(repo, tag) {
                    try {
                        const response = await fetch('/api/repositories/' + repo + '/tags/' + tag, {
                            method: 'DELETE'
                        });
                        
                        if (response.ok) {
                            this.showNotification('Tag "' + tag + '" deleted successfully');
                            setTimeout(() => location.reload(), 1000);
                        } else {
                            this.showNotification('Failed to delete tag', 'error');
                        }
                    } catch (error) {
                        this.showNotification('Failed to delete tag', 'error');
                    }
                },

                deleteRepository(repo) {
                    this.showModal(
                        'Delete Repository',
                        'Are you sure you want to delete repository "' + repo + '"? This will delete all tags and images.',
                        () => this.performDeleteRepository(repo)
                    );
                },

                async performDeleteRepository(repo) {
                    try {
                        const response = await fetch('/api/repositories/' + repo, {
                            method: 'DELETE'
                        });
                        
                        if (response.ok) {
                            this.showNotification('Repository "' + repo + '" deleted successfully');
                            setTimeout(() => location.reload(), 1000);
                        } else {
                            this.showNotification('Failed to delete repository', 'error');
                        }
                    } catch (error) {
                        this.showNotification('Failed to delete repository', 'error');
                    }
                }
            }
        }
    </script>
</body>
</html>`
