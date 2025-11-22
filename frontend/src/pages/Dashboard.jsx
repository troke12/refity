import { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { dashboardAPI, repositoriesAPI, authAPI } from '../services/api';
import { formatBytes } from '../utils/formatBytes';
import StatCard from '../components/StatCard';
import RepositoryList from '../components/RepositoryList';
import CreateRepositoryModal from '../components/CreateRepositoryModal';
import './Dashboard.css';

function Dashboard() {
  const [data, setData] = useState(null);
  const [isLoading, setIsLoading] = useState(true);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [showCreateModal, setShowCreateModal] = useState(false);
  const navigate = useNavigate();

  const loadData = async () => {
    try {
      const dashboardData = await dashboardAPI.get();
      setData(dashboardData);
    } catch (error) {
      console.error('Failed to load dashboard:', error);
    } finally {
      setIsLoading(false);
      setIsRefreshing(false);
    }
  };

  useEffect(() => {
    loadData();
  }, []);

  const handleRefresh = () => {
    setIsRefreshing(true);
    loadData();
  };

  const handleLogout = async () => {
    if (window.confirm('Are you sure you want to logout?')) {
      await authAPI.logout();
      navigate('/login');
    }
  };

  const handleRepositoryCreated = () => {
    setShowCreateModal(false);
    loadData();
  };

  const handleRepositoryDeleted = () => {
    loadData();
  };

  const handleTagDeleted = () => {
    loadData();
  };

  if (isLoading) {
    return (
      <div className="d-flex justify-content-center align-items-center" style={{ minHeight: '100vh' }}>
        <div className="spinner-border text-primary" role="status">
          <span className="visually-hidden">Loading...</span>
        </div>
      </div>
    );
  }

  return (
    <div className="container-main">
      <nav className="navbar navbar-expand-lg navbar-dark mb-4">
        <div className="container-fluid">
          <span className="navbar-brand">
            <i className="bi bi-box-seam me-2"></i>Refity Docker Registry
          </span>
          <div className="navbar-nav ms-auto d-flex flex-row gap-2">
            <button
              onClick={handleRefresh}
              className="btn btn-outline-light"
              disabled={isRefreshing}
            >
              {isRefreshing ? (
                <span className="spinner-border spinner-border-sm me-2"></span>
              ) : (
                <i className="bi bi-arrow-clockwise me-1"></i>
              )}
              Refresh
            </button>
            <button onClick={handleLogout} className="btn btn-outline-light">
              <i className="bi bi-box-arrow-right me-1"></i>Logout
            </button>
          </div>
        </div>
      </nav>

      <div className="container-fluid px-0">
        <div className="row mb-4 g-3">
          <div className="col-md-4">
            <StatCard
              icon="bi-images"
              label="Total Images"
              value={data?.total_images || 0}
              gradient="primary"
            />
          </div>
          <div className="col-md-4">
            <StatCard
              icon="bi-folder"
              label="Total Repositories"
              value={data?.repositories?.length || 0}
              gradient="success"
            />
          </div>
          <div className="col-md-4">
            <StatCard
              icon="bi-hdd"
              label="Total Size"
              value={formatBytes(data?.total_size || 0)}
              gradient="warning"
            />
          </div>
        </div>

        <div className="mb-4">
          <button
            onClick={() => setShowCreateModal(true)}
            className="btn btn-primary"
          >
            <i className="bi bi-plus-circle me-2"></i>Create New Repository
          </button>
        </div>

        <RepositoryList
          repositories={data?.repositories || []}
          onRepositoryDeleted={handleRepositoryDeleted}
          onTagDeleted={handleTagDeleted}
        />
      </div>

      <CreateRepositoryModal
        show={showCreateModal}
        onClose={() => setShowCreateModal(false)}
        onCreated={handleRepositoryCreated}
      />
    </div>
  );
}

export default Dashboard;

